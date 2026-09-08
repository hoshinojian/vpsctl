package main

// accounts.go：CLI 账号管理（list/add/edit/remove），与 webui 的账号管理
// 操作同一份 accounts.json、同一套规则（最后一个账号不可删等）。
// 注意：serve 运行中修改配置文件不会热生效，请在管理台里操作或重启 serve。

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/hoshinojian/vpsctl/internal/config"
)

func runAccounts(args []string) error {
	if len(args) == 0 {
		return errors.New("用法: vpsctl accounts list|add|edit|remove [flags]（-h 查看各自参数）")
	}
	switch args[0] {
	case "list":
		return accountsList(args[1:])
	case "add":
		return accountsAdd(args[1:])
	case "edit":
		return accountsEdit(args[1:])
	case "remove":
		return accountsRemove(args[1:])
	default:
		return fmt.Errorf("未知操作 %q（可选 list | add | edit | remove）", args[0])
	}
}

func accountsList(args []string) error {
	fs := flag.NewFlagSet("accounts list", flag.ExitOnError)
	accounts := fs.String("accounts", "", "账号配置路径（默认 ~/.config/vpsctl/accounts.json，或 $VPSCTL_ACCOUNTS）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _, err := loadAccounts(*accounts)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "名称\t提供商\tSSH用户\t密码")
	for _, a := range cfg.Accounts {
		user := a.SSHUser
		if user == "" {
			user = "root"
		}
		pw := "无"
		if a.SSHPassword != "" {
			pw = "已配置"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Name, a.Provider, user, pw)
	}
	return tw.Flush()
}

// editFlags 抽出 add/edit 共用的凭据字段。
type editFlags struct {
	token     string
	sshUser   string
	sshPass   string
	clearPass bool
}

func registerEditFlags(fs *flag.FlagSet, ef *editFlags, editing bool) {
	tokenLabel := "API Token（必填）"
	if editing {
		tokenLabel = "新 API Token（留空 = 不变）"
	}
	fs.StringVar(&ef.token, "token", "", tokenLabel)
	fs.StringVar(&ef.sshUser, "ssh-user", "", "SSH 用户（编辑时留空 = 不变；新增留空 = root）")
	fs.StringVar(&ef.sshPass, "ssh-password", "", "SSH 密码（编辑时留空 = 不变）")
	if editing {
		fs.BoolVar(&ef.clearPass, "clear-password", false, "清除已配置的 SSH 密码")
	}
}

func accountsAdd(args []string) error {
	fs := flag.NewFlagSet("accounts add", flag.ExitOnError)
	accounts := fs.String("accounts", "", "账号配置路径")
	name := fs.String("name", "", "账号名（唯一，必填）")
	providerName := fs.String("provider", "digitalocean", "提供商名")
	var ef editFlags
	registerEditFlags(fs, &ef, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || ef.token == "" {
		return errors.New("--name 与 --token 必填")
	}
	cfg, path, err := loadAccounts(*accounts)
	if err != nil {
		return err
	}
	if cfg.Find(*name) != nil {
		return fmt.Errorf("账号 %q 已存在", *name)
	}
	cfg.Accounts = append(cfg.Accounts, config.Account{
		Name: *name, Provider: *providerName, Token: ef.token,
		SSHUser: ef.sshUser, SSHPassword: ef.sshPass,
	})
	if err := config.Save(path, cfg); err != nil {
		return err
	}
	fmt.Printf("账号 %s 已新增（serve 运行中请在管理台操作或重启后生效）\n", *name)
	return nil
}

func accountsEdit(args []string) error {
	fs := flag.NewFlagSet("accounts edit", flag.ExitOnError)
	accounts := fs.String("accounts", "", "账号配置路径")
	name := fs.String("name", "", "要编辑的账号名（必填）")
	var ef editFlags
	registerEditFlags(fs, &ef, true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("--name 必填")
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !set["token"] && !set["ssh-user"] && !set["ssh-password"] && !set["clear-password"] {
		return errors.New("没有提供任何要修改的字段（--token/--ssh-user/--ssh-password/--clear-password）")
	}
	cfg, path, err := loadAccounts(*accounts)
	if err != nil {
		return err
	}
	a := cfg.Find(*name)
	if a == nil {
		return fmt.Errorf("账号 %q 不存在", *name)
	}
	if set["token"] && ef.token != "" {
		a.Token = ef.token
	}
	if set["ssh-user"] {
		a.SSHUser = ef.sshUser
	}
	if ef.clearPass {
		a.SSHPassword = ""
	} else if ef.sshPass != "" {
		a.SSHPassword = ef.sshPass
	}
	if err := config.Save(path, cfg); err != nil {
		return err
	}
	fmt.Printf("账号 %s 已更新（serve 运行中请在管理台操作或重启后生效）\n", *name)
	return nil
}

func accountsRemove(args []string) error {
	fs := flag.NewFlagSet("accounts remove", flag.ExitOnError)
	accounts := fs.String("accounts", "", "账号配置路径")
	name := fs.String("name", "", "要删除的账号名（必填）")
	force := fs.Bool("force", false, "账号下仍有节点时强制删除配置")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("--name 必填")
	}
	cfg, path, err := loadAccounts(*accounts)
	if err != nil {
		return err
	}
	a := cfg.Find(*name)
	if a == nil {
		return fmt.Errorf("账号 %q 不存在", *name)
	}
	// 防呆：账号下仍有机器时要求 --force（只删本地配置，机器不受影响）
	if !*force {
		clients, err := buildClients(&config.File{Accounts: []config.Account{*a}})
		if err == nil {
			if servers, err := clients[0].Provider.List(bgContext()); err == nil && len(servers) > 0 {
				return fmt.Errorf("账号 %s 下仍有 %d 台节点（删除配置后它们将脱离管理）；确认请加 --force", *name, len(servers))
			}
		}
	}
	if err := cfg.Remove(*name); err != nil {
		return err
	}
	if err := config.Save(path, cfg); err != nil {
		return err
	}
	fmt.Printf("账号 %s 已删除\n", *name)
	return nil
}
