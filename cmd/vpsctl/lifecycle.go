package main

// lifecycle.go：CLI 生命周期子命令（delete / power），与 webui 共用
// fleet 的批操作实现——防误删语义（数量确认、宁可漏删）两端一致。

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/hoshinojian/vpsctl/internal/fleet"
	"github.com/hoshinojian/vpsctl/internal/provider"
)

// signalContext 与其他子命令一致：Ctrl-C 触发取消。
func signalContext() (context.Context, func()) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

// resolveTargets 按 --tag / --ids（account/id 逗号分隔）解析操作目标，
// 两者互斥且必给其一。解析需要实时拉取各账号节点；任一账号查询失败即中止
// （破坏性操作宁可不动手）。
func resolveTargets(ctx context.Context, clients []fleet.AccountClient, tag, idsCSV string) ([]fleet.Target, error) {
	tag, idsCSV = strings.TrimSpace(tag), strings.TrimSpace(idsCSV)
	switch {
	case tag != "" && idsCSV != "":
		return nil, errors.New("--tag 与 --ids 只能给一个")
	case tag == "" && idsCSV == "":
		return nil, errors.New("需要 --tag 或 --ids 指定操作目标（可用 list 查看节点）")
	}
	var servers []provider.Server
	for _, ac := range clients {
		ss, err := ac.Provider.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("账号 %s 查询节点失败: %w（破坏性操作要求全部账号可查询）", ac.Name, err)
		}
		servers = append(servers, ss...)
	}
	if tag != "" {
		return fleet.SelectByTag(servers, tag), nil
	}
	return fleet.SelectByIDs(servers, splitCSV(idsCSV))
}

// confirmCount 交互式确认台数：--confirm 给了直接校验；否则从 stdin 读。
func confirmCount(n int, flagValue int) error {
	got := flagValue
	if got == 0 {
		fmt.Printf("将操作 %d 台。请输入台数确认（ Ctrl-C 取消）: ", n)
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return fmt.Errorf("读取确认输入: %w", err)
		}
		v, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			return errors.New("确认输入不是数字，已取消")
		}
		got = v
	}
	if got != n {
		return fmt.Errorf("确认台数 %d 与实际 %d 不一致，未执行", got, n)
	}
	return nil
}

func runDelete(args []string) error {
	fs := flag.NewFlagSet("delete", flag.ExitOnError)
	accounts := fs.String("accounts", "", "账号配置路径（默认 ~/.config/vpsctl/accounts.json，或 $VPSCTL_ACCOUNTS）")
	only := fs.String("only", "", "逗号分隔的账号名：只在指定账号范围内选目标（默认全部账号）")
	tag := fs.String("tag", "", "按 tag 选目标，如 batch:20260908T120000Z")
	ids := fs.String("ids", "", "按 account/id 精确选目标，逗号分隔")
	confirm := fs.Int("confirm", 0, "目标台数确认（与所选台数一致才执行；不给则交互输入）")
	shutdownFirst := fs.Bool("shutdown-first", false, "先优雅关机，关机未完成则不删（宁可漏删）")
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
	res := fleet.DeleteBatch(ctx, clients, targets, fleet.DeleteOptions{ShutdownFirst: *shutdownFirst})
	if err := printJSON(res, *output); err != nil {
		return err
	}
	return failOnOpErrors(res)
}

func runPower(args []string) error {
	fs := flag.NewFlagSet("power", flag.ExitOnError)
	accounts := fs.String("accounts", "", "账号配置路径（默认 ~/.config/vpsctl/accounts.json，或 $VPSCTL_ACCOUNTS）")
	only := fs.String("only", "", "逗号分隔的账号名：只在指定账号范围内选目标（默认全部账号）")
	tag := fs.String("tag", "", "按 tag 选目标")
	ids := fs.String("ids", "", "按 account/id 精确选目标，逗号分隔")
	action := fs.String("action", "", "电源动作：off（硬断电）| on（开机）（必填）")
	output := fs.String("output", "", "结果 JSON 另存路径（stdout 始终输出）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var act string
	switch *action {
	case "off":
		act = provider.PowerOff
	case "on":
		act = provider.PowerOn
	default:
		return errors.New("--action 必填：off | on")
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
	res := fleet.PowerBatch(ctx, clients, targets, act)
	if err := printJSON(res, *output); err != nil {
		return err
	}
	return failOnOpErrors(res)
}

// failOnOpErrors 有失败台数时以非零退出（结果 JSON 已完整输出）。
func failOnOpErrors(res []fleet.OpResult) error {
	fails := 0
	for _, x := range res {
		if !x.OK {
			fails++
		}
	}
	if fails > 0 {
		return fmt.Errorf("%d 台操作失败（详见上方 JSON）", fails)
	}
	return nil
}

// bgContext 无中断信号的朴素后台上下文（短命令用）。
func bgContext() context.Context { return context.Background() }
