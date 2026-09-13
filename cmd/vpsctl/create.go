package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/hoshinojian/vpsctl/internal/config"
	"github.com/hoshinojian/vpsctl/internal/fleet"
)

func runCreate(args []string) error {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	accounts := fs.String("accounts", "", "账号配置路径（默认 ~/.config/vpsctl/accounts.json，或 $VPSCTL_ACCOUNTS）")
	count := fs.Int("count", 1, "每个账号创建台数")
	prefix := fs.String("name-prefix", "vps", "节点名前缀，命名 {prefix}-{account}-{region}-{NN}")
	start := fs.Int("start-index", 1, "序号起始（显式给出即按它起号，覆盖语义不盘点；缺省盘点各选中账号现有节点自动避让续号）")
	region := fs.String("region", "", "区域 slug，如 sgp1（必填）")
	size := fs.String("size", "", "套餐 slug，如 s-1vcpu-1gb（必填）")
	image := fs.String("image", "", "镜像 slug，如 ubuntu-24-04-x64（必填）")
	sshKeys := fs.String("ssh-keys", "", "逗号分隔：公钥 ID / 指纹 / 名称")
	tags := fs.String("tags", "", "逗号分隔的附加 tag（自动追加 batch:<时间戳>）")
	userData := fs.String("user-data", "", "cloud-init 文件路径")
	wait := fs.Duration("wait", 0, "等待节点 active 且公网 IPv4 就绪的最长时间（如 300s；0 不等待）")
	only := fs.String("only", "", "逗号分隔的账号名：只在指定账号上创建（默认全部账号）")
	dryRun := fs.Bool("dry-run", false, "只打印创建计划，不调任何 API")
	allowBare := fs.Bool("allow-bare", false, "放行「无密码且无 user-data/ssh-keys」的裸机创建（缺省 fail-fast，防 P81 裸机交付）")
	output := fs.String("output", "", "结果 JSON 另存路径（stdout 始终输出）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, _, err := loadAccounts(*accounts)
	if err != nil {
		return err
	}
	clients, err := buildClients(cfg)
	if err != nil {
		return err
	}

	clients, err = fleet.SelectClients(clients, splitCSV(*only))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	startIdx, err := resolveStartIndex(ctx, clients, fs, *start, *prefix, *region)
	if err != nil {
		return err
	}

	opts := fleet.Options{
		Clients:     clients,
		Count:       *count,
		Prefix:      *prefix,
		StartIndex:  startIdx,
		Region:      *region,
		Size:        *size,
		Image:       *image,
		SSHKeys:     splitCSV(*sshKeys),
		ExtraTags:   splitCSV(*tags),
		Monitoring:  true,
		WaitTimeout: *wait,
	}
	if *userData != "" {
		b, err := os.ReadFile(*userData)
		if err != nil {
			return fmt.Errorf("读取 user-data: %w", err)
		}
		opts.UserData = string(b)
	}
	if err := injectPasswords(cfg, clients, *userData, splitCSV(*sshKeys), *allowBare, &opts); err != nil {
		return err
	}

	if *dryRun {
		plan, err := fleet.Plan(opts)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{
			"batch":   fleet.BatchTag(time.Now()),
			"dry_run": true,
			"plan":    plan,
		}, *output)
	}

	res, err := fleet.Create(ctx, opts)
	if err != nil {
		return err
	}
	if err := printJSON(res, *output); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("%d 台创建失败（部分节点可能已创建，详见上方 JSON）", len(res.Errors))
	}
	return nil
}

// resolveStartIndex 决定 create 的起始序号。用户显式给了 -start-index 则原样
// 返回（覆盖语义，完全不盘点）；未显式给时对各选中账号盘点现有节点，取各账号
// NextStartIndex 的 max 作全局起始——名字含 account 本不跨账号碰撞，避让的是
// 同账号批间序号空洞。任一账号 List 失败即 fail-fast：学 webui 的静默回退会在
// 既有节点上重名，NMS 以名字作节点 id 会整单拒绝。
func resolveStartIndex(ctx context.Context, clients []fleet.AccountClient, fs *flag.FlagSet, explicit int, prefix, region string) (int, error) {
	explicitSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "start-index" {
			explicitSet = true
		}
	})
	if explicitSet {
		return explicit, nil
	}
	if prefix == "" || region == "" {
		return 1, nil // 缺前缀/区域的参数错误交给 Options.validate，别白打 API
	}
	start := 1
	for _, c := range clients {
		servers, err := c.Provider.List(ctx)
		if err != nil {
			return 0, fmt.Errorf("账号 %s 盘点现有节点失败（自动避让要求全部账号可查询；也可显式传 -start-index 跳过盘点）: %w", c.Name, err)
		}
		if n := fleet.NextStartIndex(servers, prefix, c.Name, region); n > start {
			start = n
		}
	}
	return start, nil
}

// injectPasswords 把选中账号配置的 ssh_password 生成为账号专属 cloud-init
// （设密码 + 开 SSH 密码登录）。与 --user-data 互斥：零依赖没有 YAML 合并
// 能力，两者同给宁可报错，避免密码悄悄不生效。
//
// V1 防呆（92 台轮 P81 裸机交付——无密码账号 + 不传 user-data/ssh-keys = 机器无法
// SSH 管理，探针负事实升级后拉黑死锁）：账号无 ssh_password ∧ 无 --user-data ∧
// 无 --ssh-keys 时缺省 fail-fast；--allow-bare 显式放行（降级为 stderr 警告留痕）。
func injectPasswords(cfg *config.File, clients []fleet.AccountClient, userDataPath string, sshKeys []string, allowBare bool, opts *fleet.Options) error {
	byName := make(map[string]config.Account, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		byName[a.Name] = a
	}
	var bare []string
	for _, c := range clients {
		a := byName[c.Name]
		if a.SSHPassword == "" {
			if userDataPath == "" && len(sshKeys) == 0 {
				bare = append(bare, c.Name)
			}
			continue
		}
		if userDataPath != "" {
			return fmt.Errorf("账号 %s 配置了 ssh_password，与 --user-data 互斥：请把密码并入该 cloud-config 后去掉其一", c.Name)
		}
		ud, err := fleet.CloudInitPassword(a.SSHUser, a.SSHPassword)
		if err != nil {
			return fmt.Errorf("账号 %s: %w", c.Name, err)
		}
		if opts.UserDataByAccount == nil {
			opts.UserDataByAccount = map[string]string{}
		}
		opts.UserDataByAccount[c.Name] = ud
	}
	if len(bare) > 0 {
		msg := fmt.Sprintf("账号 %v 无 ssh_password 且未提供 --user-data/--ssh-keys：机器将无法通过密码/密钥 SSH 管理", bare)
		if !allowBare {
			return fmt.Errorf("%s——确认要裸机创建请加 --allow-bare", msg)
		}
		fmt.Fprintf(os.Stderr, "警告：%s（--allow-bare 放行）\n", msg)
	}
	return nil
}

// printJSON 输出缩进 JSON 到 stdout，path 非空时另存一份。
func printJSON(v any, path string) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := os.Stdout.Write(data); err != nil {
		return err
	}
	if path == "" {
		return nil
	}
	return os.WriteFile(path, data, 0o600)
}
