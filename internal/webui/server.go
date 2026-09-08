// Package webui 实现本地 Web 管理台：跨账号节点列表 + 关机/开机/删除。
// token 只留在后端内存，任何 API 响应都不包含凭据。
package webui

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hoshinojian/vpsctl/internal/config"
	"github.com/hoshinojian/vpsctl/internal/fleet"
	"github.com/hoshinojian/vpsctl/internal/nms"
	"github.com/hoshinojian/vpsctl/internal/provider"
)

//go:embed static/index.html
var indexHTML []byte

// Server 是管理台 HTTP 服务。
type Server struct {
	mu        sync.RWMutex // 保护下面三样（新增账号会热更新 clients/byAccount/cfg）
	clients   []fleet.AccountClient
	byAccount map[string]provider.Provider
	cfg       *config.File // 账号配置（NMS 导出凭据来源 + 新增账号落盘）
	cfgPath   string

	// newProvider 可注入（测试）；生产为 provider.New
	newProvider func(providerName, account, token string) (provider.Provider, error)

	deleteWait time.Duration // shutdown_first 等待关机完成上限
	pollEvery  time.Duration
}

func New(clients []fleet.AccountClient, cfg *config.File, cfgPath string) *Server {
	byAccount := make(map[string]provider.Provider, len(clients))
	for _, ac := range clients {
		byAccount[ac.Name] = ac.Provider
	}
	return &Server{
		clients:   clients,
		byAccount: byAccount,
		cfg:       cfg,
		cfgPath:   cfgPath,
		newProvider: func(name, account, token string) (provider.Provider, error) {
			return provider.New(name, account, token, nil)
		},
		deleteWait: 120 * time.Second,
		pollEvery:  2 * time.Second,
	}
}

// snapshot 取当前账号客户端/提供商映射/配置的一致性视图。
func (s *Server) snapshot() ([]fleet.AccountClient, map[string]provider.Provider, *config.File) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clients, s.byAccount, s.cfg
}

// Handler 返回路由，便于 httptest。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/droplets", s.handleDroplets)
	mux.HandleFunc("GET /api/catalog", s.handleCatalog)
	mux.HandleFunc("POST /api/create", s.handleCreate)
	mux.HandleFunc("POST /api/power", s.handlePower)
	mux.HandleFunc("POST /api/delete", s.handleDelete)
	mux.HandleFunc("GET /api/accounts", s.handleAccounts)
	mux.HandleFunc("POST /api/accounts", s.handleAddAccount)
	mux.HandleFunc("GET /api/nms-payload", s.handleNMSPayload)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	return logMiddleware(mux)
}

// ---- API 结构 ----

type target struct {
	Account string `json:"account"`
	ID      string `json:"id"`
}

type uiDroplet struct {
	fleet.ServerJSON
	Provider string `json:"provider"`
}

type accountError struct {
	Account string `json:"account"`
	Error   string `json:"error"`
}

type listResponse struct {
	Droplets []uiDroplet    `json:"droplets"`
	Errors   []accountError `json:"errors"`
}

type opResult struct {
	Account string `json:"account"`
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

type resultsResponse struct {
	Results []opResult `json:"results"`
}

type powerRequest struct {
	Targets []target `json:"targets"`
	Action  string   `json:"action"`
}

type deleteRequest struct {
	Targets       []target `json:"targets"`
	Count         int      `json:"count"` // 必须等于 len(targets)，防误操作
	ShutdownFirst bool     `json:"shutdown_first"`
}

// ---- handlers ----

// maxCreateCount 限制单次 UI 创建台数，防误操作。
const maxCreateCount = 50

type createRequest struct {
	Account string   `json:"account"`
	Count   int      `json:"count"`
	Region  string   `json:"region"`
	Size    string   `json:"size"`
	Image   string   `json:"image"`
	SSHKeys []string `json:"ssh_keys"` // 公钥 ID / 指纹
	Prefix  string   `json:"prefix"`
}

// handleCreate 在选定账号上创建节点：复用 fleet.Create 编排（命名/批 tag/逐台结果），
// 起始序号按该账号现有节点自动续号避免重名；不等待就绪，列表刷新可见 new → active。
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Account == "" {
		httpError(w, http.StatusBadRequest, "account 必填")
		return
	}
	if req.Count < 1 || req.Count > maxCreateCount {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("count 需在 1-%d", maxCreateCount))
		return
	}
	if req.Region == "" || req.Size == "" || req.Image == "" {
		httpError(w, http.StatusBadRequest, "region/size/image 必填")
		return
	}
	if req.Prefix == "" {
		req.Prefix = "vps"
	}
	clients, _, _ := s.snapshot()
	clients, err := fleet.SelectClients(clients, []string{req.Account})
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	start := 1
	if servers, err := clients[0].Provider.List(r.Context()); err == nil {
		start = fleet.NextStartIndex(servers, req.Prefix, req.Account)
	}
	res, err := fleet.Create(r.Context(), fleet.Options{
		Clients: clients, Count: req.Count, Prefix: req.Prefix, StartIndex: start,
		Region: req.Region, Size: req.Size, Image: req.Image,
		SSHKeys: req.SSHKeys, Monitoring: true,
	})
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type catalogResponse struct {
	Accounts []string          `json:"accounts"`
	Regions  []provider.Region `json:"regions,omitempty"`
	Sizes    []provider.Size   `json:"sizes,omitempty"`
	Images   []provider.Image  `json:"images,omitempty"`
	Keys     []provider.SSHKey `json:"keys,omitempty"`
	Errors   map[string]string `json:"errors,omitempty"`
}

// handleCatalog 返回创建表单可选值：无 account 参数时只给账号列表；
// 带账号时并行拉取该账号的 regions/sizes/images/keys（均为只读 API）。
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	account := r.URL.Query().Get("account")
	clients, byAccount, _ := s.snapshot()
	names := make([]string, 0, len(clients))
	for _, ac := range clients {
		names = append(names, ac.Name)
	}
	sort.Strings(names)
	resp := catalogResponse{Accounts: names}
	if account == "" {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	p, ok := byAccount[account]
	if !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("未知账号 %q（可用: %s）", account, strings.Join(names, ", ")))
		return
	}
	var (
		regions []provider.Region
		sizes   []provider.Size
		images  []provider.Image
		keys    []provider.SSHKey
	)
	errs := map[string]string{}
	var wg sync.WaitGroup
	run := func(name string, fn func() error) {
		defer wg.Done()
		if err := fn(); err != nil {
			errs[name] = err.Error()
		}
	}
	wg.Add(4)
	go run("regions", func() error { var e error; regions, e = p.Regions(r.Context()); return e })
	go run("sizes", func() error { var e error; sizes, e = p.Sizes(r.Context()); return e })
	go run("images", func() error { var e error; images, e = p.Images(r.Context()); return e })
	go run("keys", func() error { var e error; keys, e = p.SSHKeys(r.Context()); return e })
	wg.Wait()

	resp.Regions, resp.Sizes, resp.Images, resp.Keys = regions, sizes, images, keys
	if len(errs) > 0 {
		resp.Errors = errs
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDroplets(w http.ResponseWriter, r *http.Request) {
	clients, _, _ := s.snapshot()
	out := make([][]uiDroplet, len(clients))
	errs := make([]accountError, len(clients))
	var wg sync.WaitGroup
	for i, ac := range clients {
		wg.Add(1)
		go func(i int, ac fleet.AccountClient) {
			defer wg.Done()
			servers, err := ac.Provider.List(r.Context())
			if err != nil {
				errs[i] = accountError{Account: ac.Name, Error: err.Error()}
				return
			}
			for _, sv := range servers {
				out[i] = append(out[i], uiDroplet{ServerJSON: toServerJSON(sv), Provider: ac.ProviderName})
			}
		}(i, ac)
	}
	wg.Wait()

	resp := listResponse{}
	for _, d := range out {
		resp.Droplets = append(resp.Droplets, d...)
	}
	for _, e := range errs {
		if e.Account != "" {
			resp.Errors = append(resp.Errors, e)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePower(w http.ResponseWriter, r *http.Request) {
	var req powerRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Action != provider.PowerOff && req.Action != provider.PowerOn {
		httpError(w, http.StatusBadRequest, "action 仅支持 power_off / power_on")
		return
	}
	if len(req.Targets) == 0 {
		httpError(w, http.StatusBadRequest, "targets 为空")
		return
	}
	results := make([]opResult, len(req.Targets))
	_, byAccount, _ := s.snapshot()
	var wg sync.WaitGroup
	for i, t := range req.Targets {
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()
			p, ok := byAccount[t.Account]
			if !ok {
				results[i] = opResult{Account: t.Account, ID: t.ID, Error: "未知账号"}
				return
			}
			if _, err := p.Power(r.Context(), t.ID, req.Action); err != nil {
				results[i] = opResult{Account: t.Account, ID: t.ID, Error: err.Error()}
				return
			}
			results[i] = opResult{Account: t.Account, ID: t.ID, OK: true}
		}(i, t)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, resultsResponse{Results: results})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req deleteRequest
	if !readJSON(w, r, &req) {
		return
	}
	// 服务端二次校验：count 与 targets 数一致，拦截界面误传
	if req.Count != len(req.Targets) {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("count(%d) 与 targets 数(%d) 不一致", req.Count, len(req.Targets)))
		return
	}
	if len(req.Targets) == 0 {
		httpError(w, http.StatusBadRequest, "targets 为空")
		return
	}
	results := make([]opResult, len(req.Targets))
	_, byAccount, _ := s.snapshot()
	var wg sync.WaitGroup
	for i, t := range req.Targets {
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()
			results[i] = s.deleteOne(r.Context(), byAccount, t, req.ShutdownFirst)
		}(i, t)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, resultsResponse{Results: results})
}

// deleteOne 删除单台；shutdown_first 时先优雅关机并等待完成，
// 未在时限内完成则不删（宁可漏删，不可误删）。
func (s *Server) deleteOne(ctx context.Context, byAccount map[string]provider.Provider, t target, shutdownFirst bool) opResult {
	fail := func(format string, args ...any) opResult {
		return opResult{Account: t.Account, ID: t.ID, Error: fmt.Sprintf(format, args...)}
	}
	p, ok := byAccount[t.Account]
	if !ok {
		return fail("未知账号 %q", t.Account)
	}
	if shutdownFirst {
		ref, err := p.Power(ctx, t.ID, provider.Shutdown)
		if err != nil {
			return fail("发起关机失败，未删除: %v", err)
		}
		deadline := time.Now().Add(s.deleteWait)
		for {
			status, err := p.ActionStatus(ctx, ref)
			if err != nil {
				return fail("查询关机状态失败，未删除: %v", err)
			}
			if status == "completed" {
				break
			}
			if status == "errored" {
				return fail("关机失败（provider 报 errored），未删除")
			}
			if time.Until(deadline) <= 0 {
				return fail("关机超时（>%v），未删除", s.deleteWait)
			}
			select {
			case <-ctx.Done():
				return fail("已取消，未删除")
			case <-time.After(s.pollEvery):
			}
		}
	}
	if err := p.Delete(ctx, t.ID); err != nil {
		return fail("删除失败: %v", err)
	}
	return opResult{Account: t.Account, ID: t.ID, OK: true}
}

// ---- 账号管理与 NMS 载荷导出 ----

// accountView 是账号的管理台视图：永不回传 token/密码明文。
type accountView struct {
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	SSHUser     string `json:"ssh_user"`
	HasPassword bool   `json:"has_password"`
}

type accountsResponse struct {
	Accounts  []accountView `json:"accounts"`
	Providers []string      `json:"providers"` // 已注册可用的提供商名
}

type addAccountRequest struct {
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Token       string `json:"token"`
	SSHUser     string `json:"ssh_user"`
	SSHPassword string `json:"ssh_password"`
}

// loopbackOnly 拦截非回环请求：账号管理与 NMS 载荷涉及凭据读写，
// 比只读列表更敏感，即使将来非回环监听也只在回环会话开放。
func loopbackOnly(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(r) {
		httpError(w, http.StatusForbidden, "账号管理仅限本机回环访问")
		return
	}
	_, _, cfg := s.snapshot()
	resp := accountsResponse{Providers: provider.Names()}
	for _, a := range cfg.Accounts {
		resp.Accounts = append(resp.Accounts, accountView{
			Name: a.Name, Provider: a.Provider, SSHUser: a.SSHUser,
			HasPassword: a.SSHPassword != "",
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAddAccount 新增账号：校验 → 落盘（0600）→ 热更新客户端，免重启生效。
// 先落盘成功再改内存，落盘失败时内存保持原状。
func (s *Server) handleAddAccount(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(r) {
		httpError(w, http.StatusForbidden, "账号管理仅限本机回环访问")
		return
	}
	var req addAccountRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name == "" || req.Token == "" {
		httpError(w, http.StatusBadRequest, "name/token 必填")
		return
	}
	p, err := s.newProvider(req.Provider, req.Name, req.Token)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.cfg.Accounts {
		if a.Name == req.Name {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("账号名 %q 已存在", req.Name))
			return
		}
	}
	s.cfg.Accounts = append(s.cfg.Accounts, config.Account{
		Name: req.Name, Provider: req.Provider, Token: req.Token,
		SSHUser: req.SSHUser, SSHPassword: req.SSHPassword,
	})
	if err := config.Save(s.cfgPath, s.cfg); err != nil {
		s.cfg.Accounts = s.cfg.Accounts[:len(s.cfg.Accounts)-1] // 回滚内存
		httpError(w, http.StatusInternalServerError, "保存账号文件失败: "+err.Error())
		return
	}
	s.clients = append(s.clients, fleet.AccountClient{
		Name: req.Name, ProviderName: req.Provider, Provider: p,
	})
	s.byAccount[req.Name] = p
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": req.Name})
}

// handleNMSPayload 生成 NMS 台账导入载荷（04 §1.1）。因含明文 SSH 密码，
// 仅限回环；无公网 IPv4 的节点跳过（NMS 要求 management_ip），
// 跳过数放 X-Vpsctl-Skipped 头供前端提示。
func (s *Server) handleNMSPayload(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(r) {
		httpError(w, http.StatusForbidden, "NMS 载荷含凭据，仅限本机回环下载")
		return
	}
	clients, _, cfg := s.snapshot()
	if account := r.URL.Query().Get("account"); account != "" {
		var err error
		clients, err = fleet.SelectClients(clients, []string{account})
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// 并行拉取各账号节点（与 handleDroplets 同模式）
	servers := make([]fleet.ServerJSON, 0, 16)
	errs := make([]accountError, len(clients))
	var wg sync.WaitGroup
	for i, ac := range clients {
		wg.Add(1)
		go func(i int, ac fleet.AccountClient) {
			defer wg.Done()
			list, err := ac.Provider.List(r.Context())
			if err != nil {
				errs[i] = accountError{Account: ac.Name, Error: err.Error()}
				return
			}
			for _, sv := range list {
				servers = append(servers, toServerJSON(sv))
			}
		}(i, ac)
	}
	wg.Wait()
	for _, e := range errs {
		if e.Account != "" {
			httpError(w, http.StatusBadGateway, fmt.Sprintf("账号 %s 查询失败: %s", e.Account, e.Error))
			return
		}
	}

	accts := make(map[string]nms.Account, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		accts[a.Name] = nms.Account{Provider: a.Provider, SSHUser: a.SSHUser, SSHPassword: a.SSHPassword}
	}
	var (
		keep    []fleet.ServerJSON
		skipped int
	)
	for _, sv := range servers {
		if sv.IPv4Public == "" {
			skipped++
			continue
		}
		keep = append(keep, sv)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="nms-nodes.json"`)
	w.Header().Set("X-Vpsctl-Skipped", strconv.Itoa(skipped))
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(nms.Payload{Nodes: nms.Nodes(keep, accts)})
}

// ---- 工具 ----

func toServerJSON(s provider.Server) fleet.ServerJSON {
	return fleet.ServerJSON{
		Account: s.Account, ID: s.ID, Name: s.Name, Status: s.Status,
		Region: s.Region, Size: s.Size, Image: s.Image,
		VCPUs: s.VCPUs, MemoryMB: s.MemoryMB, DiskGB: s.DiskGB,
		PriceMonthly: s.PriceMonthly,
		IPv4Public:   s.IPv4Public, IPv4Private: s.IPv4Private,
		Tags: s.Tags, CreatedAt: s.CreatedAt,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// readJSON 读取并解析请求体；失败时已写好错误响应。
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return false
	}
	return true
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/" { // 前端轮询太吵，只记 API
			fmt.Printf("%s %s %s (%v)\n", r.Method, r.URL.Path, r.RemoteAddr, time.Since(start).Round(time.Millisecond))
		}
	})
}
