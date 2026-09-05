package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/hoshinojian/vpsctl/internal/fleet"
)

func runCreate(args []string) error {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	accounts := fs.String("accounts", "", "账号配置路径（默认 ~/.config/vpsctl/accounts.json，或 $VPSCTL_ACCOUNTS）")
	count := fs.Int("count", 1, "每个账号创建台数")
	prefix := fs.String("name-prefix", "vps", "节点名前缀，命名 {prefix}-{account}-{NN}")
	start := fs.Int("start-index", 1, "序号起始（跨批次避让重名）")
	region := fs.String("region", "", "区域 slug，如 sgp1（必填）")
	size := fs.String("size", "", "套餐 slug，如 s-1vcpu-1gb（必填）")
	image := fs.String("image", "", "镜像 slug，如 ubuntu-24-04-x64（必填）")
	sshKeys := fs.String("ssh-keys", "", "逗号分隔：公钥 ID / 指纹 / 名称")
	tags := fs.String("tags", "", "逗号分隔的附加 tag（自动追加 batch:<时间戳>）")
	userData := fs.String("user-data", "", "cloud-init 文件路径")
	wait := fs.Duration("wait", 0, "等待节点 active 且公网 IPv4 就绪的最长时间（如 300s；0 不等待）")
	only := fs.String("only", "", "逗号分隔的账号名：只在指定账号上创建（默认全部账号）")
	dryRun := fs.Bool("dry-run", false, "只打印创建计划，不调任何 API")
	output := fs.String("output", "", "结果 JSON 另存路径（stdout 始终输出）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadAccounts(*accounts)
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

	opts := fleet.Options{
		Clients:     clients,
		Count:       *count,
		Prefix:      *prefix,
		StartIndex:  *start,
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
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
