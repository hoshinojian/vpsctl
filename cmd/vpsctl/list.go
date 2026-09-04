package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"text/tabwriter"
)

// runList 处理 regions/sizes/images/keys 四个查询子命令，
// 按账号分节输出人类可读表格。
func runList(kind string, args []string) error {
	fs := flag.NewFlagSet(kind, flag.ExitOnError)
	accounts := fs.String("accounts", "", "账号配置路径（默认 ~/.config/vpsctl/accounts.json，或 $VPSCTL_ACCOUNTS）")
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for i, ac := range clients {
		if i > 0 {
			fmt.Fprintln(tw)
		}
		fmt.Fprintf(tw, "== %s (%s) ==\n", ac.Name, ac.ProviderName)
		switch kind {
		case "regions":
			items, err := ac.Provider.Regions(ctx)
			if err != nil {
				return fmt.Errorf("账号 %s: %w", ac.Name, err)
			}
			fmt.Fprintln(tw, "SLUG\t名称")
			for _, it := range items {
				fmt.Fprintf(tw, "%s\t%s\n", it.Slug, it.Name)
			}
		case "sizes":
			items, err := ac.Provider.Sizes(ctx)
			if err != nil {
				return fmt.Errorf("账号 %s: %w", ac.Name, err)
			}
			fmt.Fprintln(tw, "SLUG\tvCPU\t内存MB\t磁盘GB\t月费USD")
			for _, it := range items {
				fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%.2f\n", it.Slug, it.VCPUs, it.MemoryMB, it.DiskGB, it.PriceMonthly)
			}
		case "images":
			items, err := ac.Provider.Images(ctx)
			if err != nil {
				return fmt.Errorf("账号 %s: %w", ac.Name, err)
			}
			fmt.Fprintln(tw, "SLUG\t发行版\t名称")
			for _, it := range items {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", it.Slug, it.Distribution, it.Name)
			}
		case "keys":
			items, err := ac.Provider.SSHKeys(ctx)
			if err != nil {
				return fmt.Errorf("账号 %s: %w", ac.Name, err)
			}
			fmt.Fprintln(tw, "ID\t名称\t指纹")
			for _, it := range items {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", it.ID, it.Name, it.Fingerprint)
			}
		}
		tw.Flush()
	}
	return nil
}
