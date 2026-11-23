package main

// tag.go：CLI tag 子命令（add | remove），给既有节点增删 tag——只动 tag，不碰电源
// 与磁盘；编排与 webui 共用 fleet。幂等：已持有的追加、未持有的摘除直接跳过，
// 所以 --dry-run 打出的计划就是真实执行会写的那几台。

import (
	"errors"
	"flag"
	"fmt"

	"github.com/hoshinojian/vpsctl/internal/fleet"
)

// tagPlanJSON 是 --dry-run 的输出：逐台列出变更前的现有 tag，便于人工核对。
type tagPlanJSON struct {
	Action  string            `json:"action"` // add | remove
	Tags    []string          `json:"tags"`
	DryRun  bool              `json:"dry_run"`
	Skipped int               `json:"skipped"` // 所选节点里已满足、不会产生写请求的台数
	Changes []fleet.TagChange `json:"changes"`
}

func runTag(args []string) error {
	if len(args) == 0 {
		return errors.New("用法: vpsctl tag add|remove [flags]（-h 查看参数）")
	}
	var action string
	switch args[0] {
	case "add", "remove":
		action = args[0]
	default:
		return fmt.Errorf("未知操作 %q（可选 add | remove）", args[0])
	}
	tagsDesc := "要追加的 tag，逗号分隔（必填；已持有的节点跳过）"
	if action == "remove" {
		tagsDesc = "要摘除的 tag，逗号分隔（必填；未持有的节点跳过）"
	}
	fs := flag.NewFlagSet("tag "+action, flag.ExitOnError)
	accounts := fs.String("accounts", "", "账号配置路径（默认 ~/.config/vpsctl/accounts.json，或 $VPSCTL_ACCOUNTS）")
	only := fs.String("only", "", "逗号分隔的账号名：只在指定账号范围内选目标")
	tag := fs.String("tag", "", "按现有 tag 选目标")
	ids := fs.String("ids", "", "按 account/id 精确选目标，逗号分隔")
	tags := fs.String("tags", "", tagsDesc)
	dryRun := fs.Bool("dry-run", false, "只打印变更计划（仅查询节点与校验，不调任何写接口）")
	output := fs.String("output", "", "结果 JSON 另存路径（stdout 始终输出）")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// 参数防线全部排在加载账号之前：给错了不读配置、不发请求
	names, err := parseTagNames(*tags)
	if err != nil {
		return err
	}
	if err := validateTargetSelector(*tag, *ids); err != nil {
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
	servers, err := resolveServers(ctx, clients, *tag, *ids)
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		fmt.Println("没有匹配的节点，未执行")
		return nil
	}
	var add, remove []string
	if action == "add" {
		add = names
	} else {
		remove = names
	}
	changes := fleet.PlanTags(servers, add, remove)
	if len(changes) == 0 {
		fmt.Printf("所选 %d 台均已满足，无需变更，未执行\n", len(servers))
		return nil
	}
	// 预检与真实执行同口径：有账号的提供商不支持改 tag、或 tag 名不合其约束，整体中止
	if err := fleet.CheckTags(clients, changes, names); err != nil {
		return err
	}
	if *dryRun {
		return printJSON(tagPlanJSON{
			Action: action, Tags: names, DryRun: true,
			Skipped: len(servers) - len(changes), Changes: changes,
		}, *output)
	}
	res := fleet.TagBatch(ctx, clients, changes)
	if err := printJSON(res, *output); err != nil {
		return err
	}
	return failOnOpErrors(res)
}

// parseTagNames 解析 --tags：逗号切分、去空白、去重保序；一个有效名都没有即报错。
func parseTagNames(s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, p := range splitCSV(s) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("--tags 必填：逗号分隔的 tag 名")
	}
	return out, nil
}
