package main

// maintenance.go：CLI 重装/改配子命令，与 webui 共用 fleet 编排。

import (
	"errors"
	"flag"
	"fmt"

	"github.com/hoshinojian/vpsctl/internal/fleet"
	"github.com/hoshinojian/vpsctl/internal/nodepass"
	"github.com/hoshinojian/vpsctl/internal/provider"
)

// loginFromStore 返回"按账号 / 实例 ID 取登录凭据"的查询函数，供需要重新下发密码的
// 提供商（轻量应用服务器）在重装时使用。取不到表示这台节点没有可下发的密码。
func loginFromStore(cfgPath string) (fleet.LoginFor, error) {
	stored, err := nodepass.Open(nodepass.PathFor(cfgPath)).Load()
	if err != nil {
		return nil, err
	}
	return func(account, id string) (provider.Login, bool) {
		e, ok := stored[nodepass.Key(account, id)]
		if !ok || e.Password == "" {
			return provider.Login{}, false
		}
		return provider.Login{User: e.User, Password: e.Password}, true
	}, nil
}

func runRebuild(args []string) error {
	fs := flag.NewFlagSet("rebuild", flag.ExitOnError)
	accounts := fs.String("accounts", "", "账号配置路径（默认 ~/.config/vpsctl/accounts.json，或 $VPSCTL_ACCOUNTS）")
	only := fs.String("only", "", "逗号分隔的账号名：只在指定账号范围内选目标")
	tag := fs.String("tag", "", "按 tag 选目标")
	ids := fs.String("ids", "", "按 account/id 精确选目标，逗号分隔")
	image := fs.String("image", "", "重装用镜像 slug（必填，如 ubuntu-24-04-x64；磁盘数据清空不可恢复）")
	confirm := fs.Int("confirm", 0, "目标台数确认（不给则交互输入）")
	wait := fs.Duration("wait", 0, "等待动作完成上限（默认 120s）")
	output := fs.String("output", "", "结果 JSON 另存路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *image == "" {
		return errors.New("--image 必填（重装会清空磁盘，请确认镜像 slug）")
	}
	cfg, cfgPath, err := loadAccounts(*accounts)
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
	ctx, stop := signalContext()
	defer stop()
	targets, err := resolveTargets(ctx, clients, *tag, *ids)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Println("没有匹配的节点，未执行")
		return nil
	}
	if err := confirmCount(len(targets), *confirm); err != nil {
		return err
	}
	loginFor, err := loginFromStore(cfgPath)
	if err != nil {
		return err
	}
	res := fleet.RebuildBatch(ctx, clients, targets, *image, *wait, 0, loginFor)
	if err := printJSON(res, *output); err != nil {
		return err
	}
	return failOnOpErrors(res)
}

func runResize(args []string) error {
	fs := flag.NewFlagSet("resize", flag.ExitOnError)
	accounts := fs.String("accounts", "", "账号配置路径（默认 ~/.config/vpsctl/accounts.json，或 $VPSCTL_ACCOUNTS）")
	only := fs.String("only", "", "逗号分隔的账号名：只在指定账号范围内选目标")
	tag := fs.String("tag", "", "按 tag 选目标")
	ids := fs.String("ids", "", "按 account/id 精确选目标，逗号分隔")
	size := fs.String("size", "", "目标套餐 slug（必填，如 s-2vcpu-2gb）")
	resizeDisk := fs.Bool("resize-disk", false, "同时扩磁盘（不可逆，只能扩不能缩）")
	confirm := fs.Int("confirm", 0, "目标台数确认（不给则交互输入）")
	wait := fs.Duration("wait", 0, "等待动作完成上限（默认 120s）")
	output := fs.String("output", "", "结果 JSON 另存路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *size == "" {
		return errors.New("--size 必填（编排为 关机→改配→开机）")
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
	ctx, stop := signalContext()
	defer stop()
	targets, err := resolveTargets(ctx, clients, *tag, *ids)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Println("没有匹配的节点，未执行")
		return nil
	}
	if err := confirmCount(len(targets), *confirm); err != nil {
		return err
	}
	res := fleet.ResizeBatch(ctx, clients, targets, *size, *resizeDisk, *wait, 0)
	if err := printJSON(res, *output); err != nil {
		return err
	}
	return failOnOpErrors(res)
}
