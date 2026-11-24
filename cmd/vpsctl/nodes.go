package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hoshinojian/vpsctl/internal/config"
	"github.com/hoshinojian/vpsctl/internal/fleet"
	"github.com/hoshinojian/vpsctl/internal/nms"
	"github.com/hoshinojian/vpsctl/internal/nodepass"
)

// nodeEntry 是 list 输出的单台节点（在结果 JSON 形态上补 provider 名）。
type nodeEntry struct {
	fleet.ServerJSON
	Provider string `json:"provider"`
}

type nodeError struct {
	Account string `json:"account"`
	Error   string `json:"error"`
}

type listResult struct {
	Droplets []nodeEntry `json:"droplets"`
	Errors   []nodeError `json:"errors,omitempty"`
}

// runNodeList 实现 list 子命令：跨账号拉取节点清单。
// 默认输出 vpsctl JSON；--format nms 输出 NMS 台账导入载荷
// （对齐 nms 仓 04 §1.1，POST /api/v1/topology，upsert 幂等），
// SSH 凭据取自 accounts.json 账号级 ssh_user/ssh_password。
func runNodeList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	accounts := fs.String("accounts", "", "账号配置路径（默认 ~/.config/vpsctl/accounts.json，或 $VPSCTL_ACCOUNTS）")
	only := fs.String("only", "", "逗号分隔的账号名：只列出指定账号（默认全部账号）")
	format := fs.String("format", "json", "输出格式：json（vpsctl 清单）| nms（NMS 导入载荷）")
	sshPort := fs.Int("ssh-port", 22, "SSH 端口（导出载荷 ssh_port 字段与连通性预检共用；高位口 fleet 传该值，缺省 22）")
	noCheckSSH := fs.Bool("no-check-ssh", false, "跳过导出前的 SSH 端口连通性预检")
	tag := fs.String("tag", "", "只保留含该 tag 的节点（如 batch:20260908T120000Z）")
	status := fs.String("status", "", "只保留该状态的节点（active/new/off…）")
	output := fs.String("output", "", "结果 JSON 另存路径（stdout 始终输出）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *format != "json" && *format != "nms" {
		return fmt.Errorf("未知格式 %q（可选 json | nms）", *format)
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

	entries, errs := listAll(context.Background(), clients)
	entries = filterNodes(entries, *tag, *status)

	ctx, stop := signalContext()
	defer stop()
	switch *format {
	case "nms":
		return printNMS(ctx, cfgPath, cfg, entries, *output, !*noCheckSSH, *sshPort)
	default:
		res := listResult{Droplets: entries}
		for _, e := range errs {
			res.Errors = append(res.Errors, nodeError{Account: e.Account, Error: e.Error})
		}
		return printJSON(res, *output)
	}
}

// listAll 并行拉取各账号节点（与 webui 同模式）；逐账号记录失败不中断。
func listAll(ctx context.Context, clients []fleet.AccountClient) ([]nodeEntry, []nodeError) {
	out := make([][]nodeEntry, len(clients))
	errs := make([]nodeError, len(clients))
	var wg sync.WaitGroup
	for i, ac := range clients {
		wg.Add(1)
		go func(i int, ac fleet.AccountClient) {
			defer wg.Done()
			servers, err := ac.Provider.List(ctx)
			if err != nil {
				errs[i] = nodeError{Account: ac.Name, Error: err.Error()}
				return
			}
			for _, sv := range servers {
				out[i] = append(out[i], nodeEntry{
					ServerJSON: fleet.ToServerJSON(sv),
					Provider:   ac.ProviderName,
				})
			}
		}(i, ac)
	}
	wg.Wait()
	merged := make([]nodeEntry, 0)
	listErrs := make([]nodeError, 0)
	for _, part := range out {
		merged = append(merged, part...)
	}
	for _, e := range errs {
		if e.Account != "" {
			listErrs = append(listErrs, e)
		}
	}
	sort.Slice(merged, func(a, b int) bool {
		if merged[a].Account != merged[b].Account {
			return merged[a].Account < merged[b].Account
		}
		return merged[a].Name < merged[b].Name
	})
	return merged, listErrs
}

func filterNodes(entries []nodeEntry, tag, status string) []nodeEntry {
	out := entries[:0:0]
	for _, e := range entries {
		if status != "" && e.Status != status {
			continue
		}
		if tag != "" && !hasTag(e.Tags, tag) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// printNMS 渲染并输出 NMS 导入载荷：无公网 IPv4 的节点跳过（NMS 校验
// management_ip 必填），没有可用 SSH 密码的节点告警（导入会被 04 §2.2 必填校验拒绝）。
// 密码逐台取：vpsctl 建机时生成的随机密码优先，没有记录的（存量节点）回落到账号级
// ssh_password。sshPort 同时用于载荷 ssh_port 字段与连通性预检。
func printNMS(ctx context.Context, cfgPath string, cfg *config.File, entries []nodeEntry, output string, checkSSH bool, sshPort int) error {
	byName := make(map[string]config.Account, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		byName[a.Name] = a
	}
	accts := make(map[string]nms.Account, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		accts[a.Name] = nms.Account{Provider: a.Provider, SSHUser: a.SSHUser, SSHPassword: a.SSHPassword}
	}

	stored, err := nodepass.Open(nodepass.PathFor(cfgPath)).Load()
	if err != nil {
		return err
	}
	nodePass := func(account, id string) (string, string, bool) {
		e, ok := stored[nodepass.Key(account, id)]
		return e.User, e.Password, ok
	}
	var (
		keep    []fleet.ServerJSON
		skipped []string
	)
	for _, e := range entries {
		if e.IPv4Public == "" {
			skipped = append(skipped, e.Name)
			continue
		}
		keep = append(keep, e.ServerJSON)
	}
	for _, name := range skipped {
		fmt.Fprintf(os.Stderr, "警告: %s 无公网 IPv4，未包含在 NMS 载荷（management_ip 必填）\n", name)
	}
	// SSH 连通性预检：目标端口不通的节点同样跳过（防止灌进 NMS 变死台账）
	if checkSSH && len(keep) > 0 {
		hosts := make([]string, 0, len(keep))
		for _, sv := range keep {
			hosts = append(hosts, sv.IPv4Public)
		}
		probe := fleet.ProbeSSHAll(ctx, hosts, sshPort, 2*time.Second)
		filtered := keep[:0]
		for _, sv := range keep {
			if err := probe[sv.IPv4Public]; err != nil {
				fmt.Fprintf(os.Stderr, "警告: %s（%s）SSH 端口不可达，未包含在 NMS 载荷: %v\n", sv.Name, sv.IPv4Public, err)
				continue
			}
			filtered = append(filtered, sv)
		}
		keep = filtered
	}
	// 留到连通性预检之后再点名：预检跳过的节点本来就不在载荷里
	var noPass []string
	for _, sv := range keep {
		if _, pw := nodePassword(byName, nodePass, sv); pw == "" {
			noPass = append(noPass, sv.Name)
		}
	}
	if len(noPass) > 0 {
		fmt.Fprintf(os.Stderr, "警告: 以下 %d 台节点没有 SSH 密码（没有建机时生成的随机密码记录，账号也没配 ssh_password），导入 NMS 会被拒绝（04 §2.2 必填）: %s\n",
			len(noPass), strings.Join(noPass, ", "))
	}
	if len(keep) == 0 {
		fmt.Fprintln(os.Stderr, "没有可导出的节点")
	}
	if output != "" {
		fmt.Fprintln(os.Stderr, "注意: 导出文件含明文 SSH 密码，已按 600 权限写入，勿提交进任何仓库")
	}

	payload := nms.Payload{Nodes: nms.Nodes(keep, accts, sshPort, nodePass)}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := os.Stdout.Write(data); err != nil {
		return err
	}
	if output == "" {
		return nil
	}
	return os.WriteFile(output, data, 0o600)
}

// nodePassword 取一台节点导出载荷时该用的凭据：建机时生成的随机密码优先，没有记录
// （存量节点、别的工具建的）回落到账号级配置。
func nodePassword(byName map[string]config.Account, nodePass nms.NodePassword, s fleet.ServerJSON) (user, password string) {
	a := byName[s.Account]
	user, password = a.SSHUser, a.SSHPassword
	if nodePass != nil {
		if u, pw, ok := nodePass(s.Account, s.ID); ok {
			password = pw
			if u != "" {
				user = u
			}
		}
	}
	return
}
