// Package webui 实现本地 Web 管理台：跨账号节点列表 + 关机/开机/删除。
// token 只留在后端内存，任何 API 响应都不包含凭据。
package webui

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

	deleteWait   time.Duration // shutdown_first 等待关机完成上限
	pollEvery    time.Duration
	probePort    int           // NMS 导出预检探测端口（默认 22；测试注入）
	probeTimeout time.Duration // 单台探测超时
	uiDir        string        // 非空时从磁盘读 index.html（开发热改；生产用内嵌）

	opLog []opEntry // 操作历史环形日志（最新在前，cap maxOpLog）
}

// SetUIDir 指定备用的前端资源目录：serve 启动时读该目录的 index.html（若存在），
// 便于开发时改前端样式免重编译；不设置则回退内嵌 indexHTML。
func (s *Server) SetUIDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uiDir = dir
}

// maxOpLog 操作历史保留条数（内存态，serve 重启即清）。
const maxOpLog = 100

// opEntry 是一条操作历史（不含任何凭据）。
type opEntry struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`   // power/delete/rebuild/resize/create/account/export
	Detail string    `json:"detail"` // 人类可读摘要
	OK     int       `json:"ok"`
	Fail   int       `json:"fail"`
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
		deleteWait:   120 * time.Second,
		pollEvery:    2 * time.Second,
		probePort:    22,
		probeTimeout: 2 * time.Second,
		opLog:        []opEntry{},
	}
}

// logOp 记录一条操作历史（最新插队首，超出 maxOpLog 截断）。
func (s *Server) logOp(kind, detail string, ok, fail int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logOpLocked(kind, detail, ok, fail)
}

// logOpLocked 同 logOp，供已持有 s.mu 写锁的路径调用（RWMutex 不可重入）。
func (s *Server) logOpLocked(kind, detail string, ok, fail int) {
	s.opLog = append([]opEntry{{Time: time.Now(), Kind: kind, Detail: detail, OK: ok, Fail: fail}}, s.opLog...)
	if len(s.opLog) > maxOpLog {
		s.opLog = s.opLog[:maxOpLog]
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
	mux.HandleFunc("POST /api/rebuild", s.handleRebuild)
	mux.HandleFunc("POST /api/resize", s.handleResize)
	mux.HandleFunc("GET /api/accounts", s.handleAccounts)
	mux.HandleFunc("POST /api/accounts", s.handleAddAccount)
	mux.HandleFunc("PUT /api/accounts/{name}", s.handleEditAccount)
	mux.HandleFunc("DELETE /api/accounts/{name}", s.handleRemoveAccount)
	mux.HandleFunc("GET /api/nms-payload", s.handleNMSPayload)
	mux.HandleFunc("GET /api/operations", s.handleOperations)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		s.mu.RLock()
		dir := s.uiDir
		s.mu.RUnlock()
		if dir != "" {
			if b, err := os.ReadFile(filepath.Join(dir, "index.html")); err == nil {
				_, _ = w.Write(b)
				return
			}
		}
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
		start = fleet.NextStartIndex(servers, req.Prefix, req.Account, req.Region)
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
	s.logOp("create", fmt.Sprintf("创建 %d 台（%s/%s）", len(res.Created), req.Region, req.Size),
		len(res.Created), len(res.Errors))
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
	clients, _, _ := s.snapshot()
	targets := make([]fleet.Target, len(req.Targets))
	for i, t := range req.Targets {
		targets[i] = fleet.Target{Account: t.Account, ID: t.ID}
	}
	res := fleet.PowerBatch(r.Context(), clients, targets, req.Action)
	ok, fail := 0, 0
	for _, x := range res {
		if x.OK {
			ok++
		} else {
			fail++
		}
	}
	s.logOp("power", fmt.Sprintf("%s %d 台", req.Action, len(res)), ok, fail)
	results := make([]opResult, len(res))
	for i, x := range res {
		results[i] = opResult{Account: x.Account, ID: x.ID, OK: x.OK, Error: x.Error}
	}
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
	clients, _, _ := s.snapshot()
	targets := make([]fleet.Target, len(req.Targets))
	for i, t := range req.Targets {
		targets[i] = fleet.Target{Account: t.Account, ID: t.ID}
	}
	res := fleet.DeleteBatch(r.Context(), clients, targets, fleet.DeleteOptions{
		ShutdownFirst: req.ShutdownFirst,
		Wait:          s.deleteWait,
		Poll:          s.pollEvery,
	})
	ok, fail := 0, 0
	for _, x := range res {
		if x.OK {
			ok++
		} else {
			fail++
		}
	}
	s.logOp("delete", fmt.Sprintf("删除 %d 台（shutdown_first=%v）", len(res), req.ShutdownFirst), ok, fail)
	results := make([]opResult, len(res))
	for i, x := range res {
		results[i] = opResult{Account: x.Account, ID: x.ID, OK: x.OK, Error: x.Error}
	}
	writeJSON(w, http.StatusOK, resultsResponse{Results: results})
}

// handleRebuild 重装选中节点（镜像必填，磁盘清空不可恢复）。
func (s *Server) handleRebuild(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Targets []target `json:"targets"`
		Image   string   `json:"image"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Image == "" {
		httpError(w, http.StatusBadRequest, "image 必填（重装会清空磁盘）")
		return
	}
	if len(req.Targets) == 0 {
		httpError(w, http.StatusBadRequest, "targets 为空")
		return
	}
	clients, _, _ := s.snapshot()
	targets := toFleetTargets(req.Targets)
	res := fleet.RebuildBatch(r.Context(), clients, targets, req.Image, s.deleteWait, s.pollEvery)
	ok, fail := 0, 0
	for _, x := range res {
		if x.OK {
			ok++
		} else {
			fail++
		}
	}
	s.logOp("rebuild", fmt.Sprintf("重装 %d 台 → %s", len(res), req.Image), ok, fail)
	results := make([]opResult, len(res))
	for i, x := range res {
		results[i] = opResult{Account: x.Account, ID: x.ID, OK: x.OK, Error: x.Error}
	}
	writeJSON(w, http.StatusOK, resultsResponse{Results: results})
}

// handleResize 改配选中节点（关机→改配→开机链由 fleet 编排）。
func (s *Server) handleResize(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Targets    []target `json:"targets"`
		Size       string   `json:"size"`
		ResizeDisk bool     `json:"resize_disk"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Size == "" {
		httpError(w, http.StatusBadRequest, "size 必填")
		return
	}
	if len(req.Targets) == 0 {
		httpError(w, http.StatusBadRequest, "targets 为空")
		return
	}
	clients, _, _ := s.snapshot()
	targets := toFleetTargets(req.Targets)
	res := fleet.ResizeBatch(r.Context(), clients, targets, req.Size, req.ResizeDisk, s.deleteWait, s.pollEvery)
	ok, fail := 0, 0
	for _, x := range res {
		if x.OK {
			ok++
		} else {
			fail++
		}
	}
	s.logOp("resize", fmt.Sprintf("改配 %d 台 → %s", len(res), req.Size), ok, fail)
	results := make([]opResult, len(res))
	for i, x := range res {
		results[i] = opResult{Account: x.Account, ID: x.ID, OK: x.OK, Error: x.Error}
	}
	writeJSON(w, http.StatusOK, resultsResponse{Results: results})
}

func toFleetTargets(ts []target) []fleet.Target {
	out := make([]fleet.Target, len(ts))
	for i, t := range ts {
		out[i] = fleet.Target{Account: t.Account, ID: t.ID}
	}
	return out
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
	s.logOpLocked("account", "新增账号 "+req.Name, 1, 0)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": req.Name})
}

// handleEditAccount 编辑账号：只更新请求中出现的字段；token 留空 = 不变；
// clear_password=true 显式清除密码。成功后重建该账号客户端并落盘。
func (s *Server) handleEditAccount(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(r) {
		httpError(w, http.StatusForbidden, "账号管理仅限本机回环访问")
		return
	}
	name := r.PathValue("name")
	var req struct {
		Token         *string `json:"token"`
		SSHUser       *string `json:"ssh_user"`
		SSHPassword   *string `json:"ssh_password"`
		ClearPassword bool    `json:"clear_password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.cfg.Find(name)
	if a == nil {
		httpError(w, http.StatusNotFound, fmt.Sprintf("账号 %q 不存在", name))
		return
	}
	old := *a
	if req.Token != nil && *req.Token != "" {
		a.Token = *req.Token
	}
	if req.SSHUser != nil {
		a.SSHUser = *req.SSHUser
	}
	if req.ClearPassword {
		a.SSHPassword = ""
	} else if req.SSHPassword != nil && *req.SSHPassword != "" {
		a.SSHPassword = *req.SSHPassword
	}
	if err := config.Save(s.cfgPath, s.cfg); err != nil {
		*a = old // 回滚内存
		httpError(w, http.StatusInternalServerError, "保存账号文件失败: "+err.Error())
		return
	}
	// token 变更才需要重建客户端；凭据字段不影响客户端
	if a.Token != old.Token {
		if p, err := s.newProvider(a.Provider, a.Name, a.Token); err == nil {
			s.byAccount[a.Name] = p
			for i := range s.clients {
				if s.clients[i].Name == a.Name {
					s.clients[i].Provider = p
				}
			}
		}
	}
	s.logOpLocked("account", "编辑账号 "+name, 1, 0)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

// handleRemoveAccount 删除账号：账号下仍有机器时 409（可用 force=1 强制）；
// 删除最后一个账号 400。落盘成功后移除内存客户端。
func (s *Server) handleRemoveAccount(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(r) {
		httpError(w, http.StatusForbidden, "账号管理仅限本机回环访问")
		return
	}
	name := r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.cfg.Find(name)
	if a == nil {
		httpError(w, http.StatusNotFound, fmt.Sprintf("账号 %q 不存在", name))
		return
	}
	// 防呆：账号下仍有机器时拒绝（删除的只是本地配置，机器仍在提供商处）
	if r.URL.Query().Get("force") != "1" {
		if p, ok := s.byAccount[name]; ok {
			if servers, err := p.List(r.Context()); err == nil && len(servers) > 0 {
				httpError(w, http.StatusConflict, fmt.Sprintf(
					"账号 %s 下仍有 %d 台节点，删除配置后它们将脱离管理；确认请加 ?force=1", name, len(servers)))
				return
			}
		}
	}
	old := s.cfg.Accounts
	if err := s.cfg.Remove(name); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := config.Save(s.cfgPath, s.cfg); err != nil {
		s.cfg.Accounts = old // 回滚内存
		httpError(w, http.StatusInternalServerError, "保存账号文件失败: "+err.Error())
		return
	}
	for i := range s.clients {
		if s.clients[i].Name == name {
			s.clients = append(s.clients[:i], s.clients[i+1:]...)
			break
		}
	}
	delete(s.byAccount, name)
	s.logOpLocked("account", "删除账号 "+name, 1, 0)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
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
	// SSH 连通性预检：22 端口不通的同样跳过（与 CLI --format nms 同语义）
	hosts := make([]string, 0, len(keep))
	for _, sv := range keep {
		hosts = append(hosts, sv.IPv4Public)
	}
	probe := fleet.ProbeSSHAll(r.Context(), hosts, s.probePort, s.probeTimeout)
	unreachable := 0
	filtered := keep[:0]
	for _, sv := range keep {
		if probe[sv.IPv4Public] != nil {
			unreachable++
			continue
		}
		filtered = append(filtered, sv)
	}
	keep = filtered

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="nms-nodes.json"`)
	w.Header().Set("X-Vpsctl-Skipped", strconv.Itoa(skipped))
	s.logOp("export", fmt.Sprintf("导出 NMS 载荷 %d 台（无 IP 跳过 %d，SSH 不可达跳过 %d）", len(keep), skipped, unreachable), 1, 0)
	w.Header().Set("X-Vpsctl-Unreachable", strconv.Itoa(unreachable))
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(nms.Payload{Nodes: nms.Nodes(keep, accts)})
}

// handleOperations 返回最近的操作历史（内存态，serve 重启即清）。
func (s *Server) handleOperations(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ops := make([]opEntry, len(s.opLog))
	copy(ops, s.opLog)
	writeJSON(w, http.StatusOK, map[string]any{"operations": ops})
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
		Lat: s.Lat, Lng: s.Lng,
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
