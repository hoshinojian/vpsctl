// Package fleet 实现跨账号批量编排：批量创建（含等待就绪与结果汇总）。
package fleet

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
)

// AccountClient 把账号与其客户端绑定（provider 接口本身不含账号名）。
type AccountClient struct {
	Name         string
	ProviderName string
	Provider     provider.Provider
}

// Options 描述一次批量创建。
type Options struct {
	Clients     []AccountClient
	Count       int    // 每账号台数
	Prefix      string // 命名前缀
	StartIndex  int    // 序号起始，默认 1
	Region      string
	Size        string
	Image       string
	SSHKeys     []string
	ExtraTags   []string // 自动追加 batch:<时间戳>
	UserData    string
	Monitoring  bool
	WaitTimeout time.Duration // >0 时等待 active 且有公网 IPv4
	PollEvery   time.Duration // 轮询间隔，默认 5s
	Concurrency int           // 单账号并发，默认 4
	Now         func() time.Time
}

// PlanEntry 是 dry-run 的单账号计划。
type PlanEntry struct {
	Account  string   `json:"account"`
	Provider string   `json:"provider"`
	Count    int      `json:"count"`
	Names    []string `json:"names"`
}

// BatchTag 生成批量标识（UTC 时间戳串），同时用作自动 tag（batch:<id>）。
func BatchTag(t time.Time) string {
	return t.UTC().Format("20060102T150405Z")
}

// SelectClients 按 only 过滤账号客户端；only 为空返回全部。
// 出现未知账号名时报错并列出可用名。
func SelectClients(clients []AccountClient, only []string) ([]AccountClient, error) {
	if len(only) == 0 {
		return clients, nil
	}
	byName := make(map[string]AccountClient, len(clients))
	names := make([]string, 0, len(clients))
	for _, c := range clients {
		byName[c.Name] = c
		names = append(names, c.Name)
	}
	sort.Strings(names)
	out := make([]AccountClient, 0, len(only))
	for _, n := range only {
		c, ok := byName[n]
		if !ok {
			return nil, fmt.Errorf("账号 %q 不存在（可用: %s）", n, strings.Join(names, ", "))
		}
		out = append(out, c)
	}
	return out, nil
}

// NextStartIndex 返回避免与现有节点重名的下一个序号：
// 扫描名为 {prefix}-{account}-{NN} 的最大 NN + 1；无匹配时为 1。
func NextStartIndex(servers []provider.Server, prefix, account string) int {
	re, err := regexp.Compile("^" + regexp.QuoteMeta(prefix) + "-" + regexp.QuoteMeta(account) + "-(\\d+)$")
	if err != nil {
		return 1
	}
	max := 0
	for _, s := range servers {
		if m := re.FindStringSubmatch(s.Name); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > max {
				max = n
			}
		}
	}
	return max + 1
}

// Names 生成单账号的节点名序列：{prefix}-{account}-{NN}。
func Names(prefix, account string, start, count int) []string {
	names := make([]string, 0, count)
	for i := 0; i < count; i++ {
		names = append(names, fmt.Sprintf("%s-%s-%02d", prefix, account, start+i))
	}
	return names
}

// Plan 生成 dry-run 计划，不发任何 API 请求。
func Plan(o Options) ([]PlanEntry, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	entries := make([]PlanEntry, 0, len(o.Clients))
	for _, ac := range o.Clients {
		entries = append(entries, PlanEntry{
			Account:  ac.Name,
			Provider: ac.ProviderName,
			Count:    o.Count,
			Names:    Names(o.Prefix, ac.Name, o.startIndex(), o.Count),
		})
	}
	return entries, nil
}

// ServerJSON 是结果 JSON 中的单台节点。
type ServerJSON struct {
	Account      string    `json:"account"`
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Status       string    `json:"status"`
	Region       string    `json:"region"`
	Size         string    `json:"size"`
	Image        string    `json:"image"`
	VCPUs        int       `json:"vcpus"`
	MemoryMB     int       `json:"memory_mb"`
	DiskGB       int       `json:"disk_gb"`
	PriceMonthly float64   `json:"price_monthly"`
	IPv4Public   string    `json:"ipv4_public,omitempty"`
	IPv4Private  string    `json:"ipv4_private,omitempty"`
	Tags         []string  `json:"tags,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// ErrorJSON 是结果 JSON 中的单条失败。
type ErrorJSON struct {
	Account string `json:"account"`
	Name    string `json:"name"`
	Index   int    `json:"index"` // 账号内序号（含 StartIndex 偏移）
	Error   string `json:"error"`
}

// Result 是批量创建的完整结果；部分失败时 created 与 errors 并存。
type Result struct {
	Batch     string         `json:"batch"`
	Requested map[string]int `json:"requested"` // account -> 台数
	Created   []ServerJSON   `json:"created"`
	Errors    []ErrorJSON    `json:"errors"`
}

// Create 批量创建。除参数错误外不返回 error，逐台失败记录在 Result.Errors；
// 等待就绪超时的节点已真实存在，会同时出现在 created 与 errors 中。
func Create(ctx context.Context, o Options) (*Result, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	batch := BatchTag(now())
	res := &Result{Batch: batch, Requested: map[string]int{}}

	outs := make([]acctOut, len(o.Clients))
	var wg sync.WaitGroup
	for i, ac := range o.Clients {
		res.Requested[ac.Name] = o.Count
		wg.Add(1)
		go func(i int, ac AccountClient) {
			defer wg.Done()
			outs[i] = createAccount(ctx, o, ac, batch)
		}(i, ac)
	}
	wg.Wait()
	for _, out := range outs {
		res.Created = append(res.Created, out.created...)
		res.Errors = append(res.Errors, out.errs...)
	}
	sort.Slice(res.Created, func(a, b int) bool {
		if res.Created[a].Account != res.Created[b].Account {
			return res.Created[a].Account < res.Created[b].Account
		}
		return res.Created[a].Name < res.Created[b].Name
	})
	sort.Slice(res.Errors, func(a, b int) bool {
		if res.Errors[a].Account != res.Errors[b].Account {
			return res.Errors[a].Account < res.Errors[b].Account
		}
		return res.Errors[a].Name < res.Errors[b].Name
	})
	return res, nil
}

// acctOut 是单账号的创建产出，按账号并发收集后合并。
type acctOut struct {
	created []ServerJSON
	errs    []ErrorJSON
}

func createAccount(ctx context.Context, o Options, ac AccountClient, batch string) acctOut {
	out := acctOut{}
	names := Names(o.Prefix, ac.Name, o.startIndex(), o.Count)
	sem := make(chan struct{}, o.concurrency())
	var mu sync.Mutex
	var wg sync.WaitGroup
	for idx, name := range names {
		wg.Add(1)
		go func(idx int, name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			s, err := ac.Provider.Create(ctx, provider.CreateRequest{
				Name: name, Region: o.Region, Size: o.Size, Image: o.Image,
				SSHKeys:    o.SSHKeys,
				Tags:       append(append([]string{}, o.ExtraTags...), "batch:"+batch),
				UserData:   o.UserData,
				Monitoring: o.Monitoring,
			})
			if err != nil {
				mu.Lock()
				out.errs = append(out.errs, ErrorJSON{
					Account: ac.Name, Name: name, Index: o.startIndex() + idx, Error: err.Error(),
				})
				mu.Unlock()
				return
			}
			if o.WaitTimeout > 0 {
				ws, werr := waitForActive(ctx, ac.Provider, s.ID, o.WaitTimeout, o.pollEvery())
				if werr != nil {
					mu.Lock()
					out.errs = append(out.errs, ErrorJSON{
						Account: ac.Name, Name: name, Index: o.startIndex() + idx,
						Error: fmt.Sprintf("节点已创建（ID %s）但未就绪: %v", s.ID, werr),
					})
					mu.Unlock()
				}
				if ws.ID != "" {
					s = ws
				}
			}
			mu.Lock()
			out.created = append(out.created, toServerJSON(s))
			mu.Unlock()
		}(idx, name)
	}
	wg.Wait()
	return out
}

// waitForActive 轮询直到 active 且有公网 IPv4，或超时/取消。
// 返回最后已知状态，便于调用方保留已创建节点信息。
func waitForActive(ctx context.Context, p provider.Provider, id string, timeout, every time.Duration) (provider.Server, error) {
	if every <= 0 {
		every = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var last provider.Server
	var lastErr error
	for {
		s, err := p.Get(ctx, id)
		if err == nil {
			last, lastErr = s, nil
			if s.Status == "active" && s.IPv4Public != "" {
				return s, nil
			}
		} else {
			lastErr = err
		}
		if ctx.Err() != nil {
			return last, ctx.Err()
		}
		d := min(every, time.Until(deadline))
		if d <= 0 {
			if lastErr != nil {
				return last, lastErr
			}
			return last, fmt.Errorf("等待 %v 超时（状态 %q）", timeout, last.Status)
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return last, ctx.Err()
		case <-t.C:
		}
	}
}

func toServerJSON(s provider.Server) ServerJSON {
	return ServerJSON{
		Account: s.Account, ID: s.ID, Name: s.Name, Status: s.Status,
		Region: s.Region, Size: s.Size, Image: s.Image,
		VCPUs: s.VCPUs, MemoryMB: s.MemoryMB, DiskGB: s.DiskGB,
		PriceMonthly: s.PriceMonthly,
		IPv4Public:   s.IPv4Public, IPv4Private: s.IPv4Private,
		Tags: s.Tags, CreatedAt: s.CreatedAt,
	}
}

func (o Options) validate() error {
	if len(o.Clients) == 0 {
		return errors.New("fleet: 未提供任何账号客户端")
	}
	if o.Count <= 0 {
		return errors.New("fleet: count 必须 > 0")
	}
	if o.Prefix == "" {
		return errors.New("fleet: 命名前缀不能为空")
	}
	if o.Region == "" || o.Size == "" || o.Image == "" {
		return errors.New("fleet: region/size/image 均必填")
	}
	return nil
}

func (o Options) startIndex() int {
	if o.StartIndex < 1 {
		return 1
	}
	return o.StartIndex
}

func (o Options) concurrency() int {
	if o.Concurrency < 1 {
		return 4
	}
	return o.Concurrency
}

func (o Options) pollEvery() time.Duration {
	if o.PollEvery <= 0 {
		return 5 * time.Second
	}
	return o.PollEvery
}
