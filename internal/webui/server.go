// Package webui 实现本地 Web 管理台：跨账号节点列表 + 关机/开机/删除。
// token 只留在后端内存，任何 API 响应都不包含凭据。
package webui

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/hoshinojian/vpsctl/internal/fleet"
	"github.com/hoshinojian/vpsctl/internal/provider"
)

//go:embed static/index.html
var indexHTML []byte

// Server 是管理台 HTTP 服务。
type Server struct {
	clients    []fleet.AccountClient
	byAccount  map[string]provider.Provider
	deleteWait time.Duration // shutdown_first 等待关机完成上限
	pollEvery  time.Duration
}

func New(clients []fleet.AccountClient) *Server {
	byAccount := make(map[string]provider.Provider, len(clients))
	for _, ac := range clients {
		byAccount[ac.Name] = ac.Provider
	}
	return &Server{
		clients:    clients,
		byAccount:  byAccount,
		deleteWait: 120 * time.Second,
		pollEvery:  2 * time.Second,
	}
}

// Handler 返回路由，便于 httptest。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/droplets", s.handleDroplets)
	mux.HandleFunc("POST /api/power", s.handlePower)
	mux.HandleFunc("POST /api/delete", s.handleDelete)
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

func (s *Server) handleDroplets(w http.ResponseWriter, r *http.Request) {
	out := make([][]uiDroplet, len(s.clients))
	errs := make([]accountError, len(s.clients))
	var wg sync.WaitGroup
	for i, ac := range s.clients {
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
	var wg sync.WaitGroup
	for i, t := range req.Targets {
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()
			p, ok := s.byAccount[t.Account]
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
	var wg sync.WaitGroup
	for i, t := range req.Targets {
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()
			results[i] = s.deleteOne(r.Context(), t, req.ShutdownFirst)
		}(i, t)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, resultsResponse{Results: results})
}

// deleteOne 删除单台；shutdown_first 时先优雅关机并等待完成，
// 未在时限内完成则不删（宁可漏删，不可误删）。
func (s *Server) deleteOne(ctx context.Context, t target, shutdownFirst bool) opResult {
	fail := func(format string, args ...any) opResult {
		return opResult{Account: t.Account, ID: t.ID, Error: fmt.Sprintf(format, args...)}
	}
	p, ok := s.byAccount[t.Account]
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
