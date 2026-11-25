package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/hoshinojian/vpsctl/internal/config"
	"github.com/hoshinojian/vpsctl/internal/fleet"
	"github.com/hoshinojian/vpsctl/internal/nodepass"
	"github.com/hoshinojian/vpsctl/internal/provider"
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
	sshPort := fs.Int("ssh-port", 0, "建机注入的 sshd 监听端口（0=不动缺省 22；非缺省值让自动生成的 cloud-init 禁 ssh.socket 并按该端口拉起 sshd）")
	tunnel443 := fs.Bool("tunnel-443", false, "建机时在远端起 stunnel4 监听 443 桥到 ssh_port（供出口代理 TUN 劫持直连 22 的本机经 443 接入；需 --ssh-port）")
	authKeys := fs.String("authorized-keys", "", "注入机器 authorized_keys 的运维公钥文件路径（每行一条；供免密运维，与节点纳管无关）")
	wait := fs.Duration("wait", 0, "等待节点 active 且公网 IPv4 就绪的最长时间（如 300s；0 不等待）")
	only := fs.String("only", "", "逗号分隔的账号名：只在指定账号上创建（默认全部账号）")
	dryRun := fs.Bool("dry-run", false, "只打印创建计划，不调任何 API")
	noPassword := fs.Bool("no-password", false, "不注入密码（缺省给每台节点生成一个随机 root 密码，记入账号配置旁边的 node-passwords.json）")
	allowBare := fs.Bool("allow-bare", false, "放行「不注入密码且无 user-data/ssh-keys」的裸机创建（缺省 fail-fast，防 P81 裸机交付）")
	output := fs.String("output", "", "结果 JSON 另存路径（stdout 始终输出）")
	if err := fs.Parse(args); err != nil {
		return err
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
		SSHPort:     *sshPort,
		Tunnel443:   *tunnel443,
		Monitoring:  true,
		WaitTimeout: *wait,
	}
	if *authKeys != "" {
		b, err := os.ReadFile(*authKeys)
		if err != nil {
			return fmt.Errorf("读取 authorized-keys: %w", err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				opts.AuthorizedKeys = append(opts.AuthorizedKeys, line)
			}
		}
	}
	if *userData != "" {
		b, err := os.ReadFile(*userData)
		if err != nil {
			return fmt.Errorf("读取 user-data: %w", err)
		}
		opts.UserData = string(b)
	}
	generated, err := injectPasswords(provisionParams{
		cfg: cfg, clients: clients, userDataPath: *userData, sshKeys: splitCSV(*sshKeys),
		sshPort: *sshPort, tunnel443: *tunnel443, authKeys: opts.AuthorizedKeys,
		allowBare: *allowBare, noPassword: *noPassword,
	}, &opts)
	if err != nil {
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

	passwords := nodepass.Open(nodepass.PathFor(cfgPath))
	if len(generated) > 0 {
		// 建机前先确认密码库写得进去：真建完才发现写不进去，密码就只留在本次进程的内存里了
		if err := passwords.CheckWritable(); err != nil {
			return fmt.Errorf("密码库不可写，未建机: %w", err)
		}
	}
	res, err := fleet.Create(ctx, opts)
	if err != nil {
		return err
	}
	if err := printJSON(res, *output); err != nil {
		return err
	}
	if err := passwords.RecordOrPrint(res.Created, generated, os.Stderr); err != nil {
		return fmt.Errorf("节点已创建，但随机密码没能记入密码库（见上方输出，请立即保存）: %w", err)
	}
	if n := len(res.Created); len(generated) > 0 && n > 0 {
		fmt.Fprintf(os.Stderr, "已为 %d 台节点生成随机 root 密码并记入 %s（查看：vpsctl password --ids <账号/实例ID>）\n", n, nodepass.PathFor(cfgPath))
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

// provisionParams 是 create 决定"每台节点怎么置备"所需的全部输入。
type provisionParams struct {
	cfg          *config.File
	clients      []fleet.AccountClient // 选中的账号
	userDataPath string                // --user-data：给了就原样下发、不注入密码
	sshKeys      []string
	sshPort      int
	tunnel443    bool
	authKeys     []string
	allowBare    bool
	noPassword   bool // --no-password：什么都不注入
}

// injectPasswords 决定每台节点拿到的 user-data，返回建机前生成的随机密码（"节点名 →
// 凭据"，建机成功后由调用方记入密码库）：
//
//   - 缺省：每台节点一个随机 root 密码，不依赖账号有没有配 ssh_password；
//   - 给了 --user-data：原样下发、不注入密码；
//   - --no-password：什么都不注入。
//
// 后两种情况下账号若配了固定密码必须拦下：导出 NMS 载荷会回落到那个固定密码，而
// 机器上并没有它。同理还有裸机保护（V1 / P81）——不注入密码、没有 user-data、没有
// ssh-keys 的机器无法 SSH 管理，缺省 fail-fast，--allow-bare 显式放行。
func injectPasswords(p provisionParams, opts *fleet.Options) (map[string]nodepass.Entry, error) {
	byName := make(map[string]config.Account, len(p.cfg.Accounts))
	for _, a := range p.cfg.Accounts {
		byName[a.Name] = a
	}
	if p.userDataPath != "" || p.noPassword {
		var bare, fixed []string
		for _, c := range p.clients {
			if byName[c.Name].SSHPassword != "" {
				fixed = append(fixed, c.Name)
			} else if p.userDataPath == "" && len(p.sshKeys) == 0 {
				bare = append(bare, c.Name)
			}
		}
		if len(fixed) > 0 {
			what := "--no-password"
			if p.userDataPath != "" {
				what = "--user-data"
			}
			return nil, fmt.Errorf("账号 %v 配置了 ssh_password，与 %s 互斥：这次建机不会把账号密码注入机器，导出 NMS 载荷却会带上它。请去掉账号的 ssh_password（新节点的密码由 vpsctl 随机生成），或改用缺省的随机密码", fixed, what)
		}
		if len(bare) > 0 {
			msg := fmt.Sprintf("账号 %v 不注入密码且未提供 --user-data/--ssh-keys：机器将无法通过密码/密钥 SSH 管理", bare)
			if !p.allowBare {
				return nil, fmt.Errorf("%s——确认要裸机创建请加 --allow-bare", msg)
			}
			fmt.Fprintf(os.Stderr, "警告：%s（--allow-bare 放行）\n", msg)
		}
		return nil, nil
	}
	specs := make(map[string]nodepass.Spec, len(p.clients))
	for _, c := range p.clients {
		a := byName[c.Name]
		specs[c.Name] = nodepass.Spec{User: a.SSHUser, ChpasswdCompat: provider.ChpasswdCompat(a.Provider)}
	}
	return nodepass.Provision(opts, specs)
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
