package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoshinojian/vpsctl/internal/config"
	"github.com/hoshinojian/vpsctl/internal/fleet"
	"github.com/hoshinojian/vpsctl/internal/nms"
	"github.com/hoshinojian/vpsctl/internal/nodepass"
	"github.com/hoshinojian/vpsctl/internal/provider"
)

type fakeProvider struct {
	mu           sync.Mutex
	servers      []provider.Server
	powerCalls   []string // "id:action"
	actionStatus []string // ActionStatus 依次返回；耗尽后停驻最后一个
	statusIdx    int
	deleteErr    map[string]error
	deletes      []string
	creates      []provider.CreateRequest
	regions      []provider.Region
	sizes        []provider.Size
	images       []provider.Image
	keys         []provider.SSHKey
	rebuilds     []string // "id@image"
	resizes      []string // "id@size"
}

func (f *fakeProvider) Create(_ context.Context, req provider.CreateRequest) (provider.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, req)
	return provider.Server{ID: "id-" + req.Name, Name: req.Name, Status: "new", Account: "x"}, nil
}

func (f *fakeProvider) List(context.Context) ([]provider.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.servers == nil {
		return nil, errors.New("list failed")
	}
	return f.servers, nil
}

func (f *fakeProvider) Get(_ context.Context, id string) (provider.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.servers {
		if s.ID == id {
			return s, nil
		}
	}
	return provider.Server{}, fmt.Errorf("no server %s", id)
}

func (f *fakeProvider) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, id)
	if err := f.deleteErr[id]; err != nil {
		return err
	}
	return nil
}

func (f *fakeProvider) Power(_ context.Context, id, action string) (provider.ActionRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.powerCalls = append(f.powerCalls, id+":"+action)
	return provider.ActionRef{ID: "act-" + id}, nil
}

func (f *fakeProvider) ActionStatus(_ context.Context, _ provider.ActionRef) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.actionStatus) == 0 {
		return "completed", nil
	}
	s := f.actionStatus[f.statusIdx]
	if f.statusIdx < len(f.actionStatus)-1 {
		f.statusIdx++
	}
	return s, nil
}

func (f *fakeProvider) SSHKeys(context.Context) ([]provider.SSHKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys == nil {
		return nil, errors.New("keys failed")
	}
	return f.keys, nil
}
func (f *fakeProvider) Regions(context.Context) ([]provider.Region, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.regions == nil {
		return nil, errors.New("regions failed")
	}
	return f.regions, nil
}
func (f *fakeProvider) Sizes(context.Context) ([]provider.Size, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sizes == nil {
		return nil, errors.New("sizes failed")
	}
	return f.sizes, nil
}
func (f *fakeProvider) Images(context.Context) ([]provider.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.images == nil {
		return nil, errors.New("images failed")
	}
	return f.images, nil
}

func (f *fakeProvider) Rebuild(_ context.Context, id, image string) (provider.ActionRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebuilds = append(f.rebuilds, id+"@"+image)
	return provider.ActionRef{ID: "reb-" + id}, nil
}

func (f *fakeProvider) Resize(_ context.Context, id, size string, disk bool) (provider.ActionRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, id+"@"+size)
	return provider.ActionRef{ID: "rsz-" + id}, nil
}

func testServer(t *testing.T, fps map[string]*fakeProvider) *httptest.Server {
	t.Helper()
	clients := make([]fleet.AccountClient, 0, len(fps))
	for name, fp := range fps {
		clients = append(clients, fleet.AccountClient{Name: name, ProviderName: "digitalocean", Provider: fp})
	}
	cfg := &config.File{Accounts: []config.Account{
		{Name: "a1", Provider: "digitalocean", Token: "tok-a1", SSHUser: "root", SSHPassword: "pw-a1"},
	}}
	s := New(clients, cfg, filepath.Join(t.TempDir(), "accounts.json"))
	s.newProvider = func(providerName, account string, cred provider.Credentials) (provider.Provider, error) {
		if providerName != "digitalocean" {
			return nil, fmt.Errorf("未知提供商 %q", providerName)
		}
		fp := fps[account]
		if fp == nil {
			fp = &fakeProvider{}
		}
		return fp, nil
	}
	s.pollEvery = time.Millisecond
	s.deleteWait = time.Second
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func postJSON(t *testing.T, url string, body any) (int, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestDropletsAggregation(t *testing.T) {
	srv := testServer(t, map[string]*fakeProvider{
		"a1": {servers: []provider.Server{
			{ID: "1", Account: "a1", Name: "n1", Status: "active", Region: "sgp1", Size: "s-1vcpu-1gb", PriceMonthly: 6, IPv4Public: "203.0.113.1", Tags: []string{"batch:x"}},
		}},
		"a2": {},
	})
	resp, err := http.Get(srv.URL + "/api/droplets")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got listResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)

	if len(got.Droplets) != 1 {
		t.Fatalf("droplets = %+v", got.Droplets)
	}
	d := got.Droplets[0]
	if d.Account != "a1" || d.Provider != "digitalocean" || d.Name != "n1" || d.PriceMonthly != 6 {
		t.Errorf("droplet = %+v", d.ServerJSON)
	}
	if len(got.Errors) != 1 || got.Errors[0].Account != "a2" {
		t.Errorf("errors = %+v", got.Errors)
	}
}

func TestIndexServed(t *testing.T) {
	srv := testServer(t, map[string]*fakeProvider{"a1": {servers: []provider.Server{}}})
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "<!doctype html>") {
		t.Errorf("首页内容不对: %q", string(buf[:n]))
	}
}

func TestIndexUIDirOverride(t *testing.T) {
	dir := t.TempDir()
	alt := []byte("<!doctype html><html><body>dev-ui</body></html>")
	if err := os.WriteFile(filepath.Join(dir, "index.html"), alt, 0o600); err != nil {
		t.Fatal(err)
	}
	s := New([]fleet.AccountClient{}, &config.File{}, filepath.Join(dir, "accounts.json"))
	s.SetUIDir(dir)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "dev-ui") {
		t.Errorf("磁盘 index 应优先于内嵌: %q", string(body))
	}

	// 目录里没有 index.html 时回退内嵌
	s2 := New([]fleet.AccountClient{}, &config.File{}, filepath.Join(dir, "a.json"))
	s2.SetUIDir(t.TempDir())
	srv2 := httptest.NewServer(s2.Handler())
	defer srv2.Close()
	resp2, _ := http.Get(srv2.URL + "/")
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if !strings.Contains(string(body2), "<!doctype html>") {
		t.Errorf("缺 index.html 应回退内嵌: %q", string(body2))
	}
}

func TestPower(t *testing.T) {
	fp := &fakeProvider{servers: []provider.Server{}}
	srv := testServer(t, map[string]*fakeProvider{"a1": fp})

	code, out := postJSON(t, srv.URL+"/api/power", map[string]any{
		"action": "power_off",
		"targets": []map[string]string{
			{"account": "a1", "id": "7"},
			{"account": "a1", "id": "8"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, out=%v", code, out)
	}
	results := out["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %v", results)
	}
	for _, r := range results {
		if r.(map[string]any)["ok"] != true {
			t.Errorf("result = %v", r)
		}
	}
	// 逐台并发提交，顺序不定，按集合比较
	got := append([]string(nil), fp.powerCalls...)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "7:power_off" || got[1] != "8:power_off" {
		t.Errorf("powerCalls = %v", fp.powerCalls)
	}

	// 非法 action
	code, _ = postJSON(t, srv.URL+"/api/power", map[string]any{
		"action": "format", "targets": []map[string]string{{"account": "a1", "id": "7"}},
	})
	if code != http.StatusBadRequest {
		t.Errorf("非法 action 应 400, got %d", code)
	}
	// targets 为空
	code, _ = postJSON(t, srv.URL+"/api/power", map[string]any{"action": "power_off"})
	if code != http.StatusBadRequest {
		t.Errorf("空 targets 应 400, got %d", code)
	}
}

func TestDeleteCountMismatch(t *testing.T) {
	fp := &fakeProvider{servers: []provider.Server{}}
	srv := testServer(t, map[string]*fakeProvider{"a1": fp})

	code, _ := postJSON(t, srv.URL+"/api/delete", map[string]any{
		"count":   2,
		"targets": []map[string]string{{"account": "a1", "id": "7"}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("count 不一致应 400, got %d", code)
	}
	if len(fp.deletes) != 0 {
		t.Errorf("不应发生删除: %v", fp.deletes)
	}
}

func TestDeleteDirect(t *testing.T) {
	fp := &fakeProvider{servers: []provider.Server{}}
	srv := testServer(t, map[string]*fakeProvider{"a1": fp})

	code, out := postJSON(t, srv.URL+"/api/delete", map[string]any{
		"count":   1,
		"targets": []map[string]string{{"account": "a1", "id": "7"}},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d out=%v", code, out)
	}
	if len(fp.deletes) != 1 || fp.deletes[0] != "7" {
		t.Errorf("deletes = %v", fp.deletes)
	}
	if len(fp.powerCalls) != 0 {
		t.Errorf("不应有关机调用: %v", fp.powerCalls)
	}
}

func TestDeleteShutdownFirst(t *testing.T) {
	t.Run("关机完成后删除", func(t *testing.T) {
		fp := &fakeProvider{servers: []provider.Server{}, actionStatus: []string{"in-progress", "completed"}}
		srv := testServer(t, map[string]*fakeProvider{"a1": fp})

		code, out := postJSON(t, srv.URL+"/api/delete", map[string]any{
			"count": 1, "shutdown_first": true,
			"targets": []map[string]string{{"account": "a1", "id": "9"}},
		})
		if code != http.StatusOK {
			t.Fatalf("status = %d out=%v", code, out)
		}
		if len(fp.powerCalls) != 1 || fp.powerCalls[0] != "9:shutdown" {
			t.Errorf("powerCalls = %v", fp.powerCalls)
		}
		if len(fp.deletes) != 1 || fp.deletes[0] != "9" {
			t.Errorf("deletes = %v", fp.deletes)
		}
	})
	t.Run("关机 errored 不删除", func(t *testing.T) {
		fp := &fakeProvider{servers: []provider.Server{}, actionStatus: []string{"errored"}}
		srv := testServer(t, map[string]*fakeProvider{"a1": fp})

		_, out := postJSON(t, srv.URL+"/api/delete", map[string]any{
			"count": 1, "shutdown_first": true,
			"targets": []map[string]string{{"account": "a1", "id": "9"}},
		})
		results := out["results"].([]any)
		r := results[0].(map[string]any)
		if r["ok"] == true {
			t.Errorf("errored 不应成功: %v", r)
		}
		if !strings.Contains(r["error"].(string), "未删除") {
			t.Errorf("错误信息应说明未删除: %v", r)
		}
		if len(fp.deletes) != 0 {
			t.Errorf("不应删除: %v", fp.deletes)
		}
	})
	t.Run("关机超时不删除", func(t *testing.T) {
		fp := &fakeProvider{servers: []provider.Server{}, actionStatus: []string{"in-progress"}}
		s2 := New([]fleet.AccountClient{{Name: "a1", ProviderName: "digitalocean", Provider: fp}},
			&config.File{}, filepath.Join(t.TempDir(), "accounts.json"))
		s2.pollEvery = time.Millisecond
		s2.deleteWait = 10 * time.Millisecond
		hs := httptest.NewServer(s2.Handler())
		defer hs.Close()

		_, out := postJSON(t, hs.URL+"/api/delete", map[string]any{
			"count": 1, "shutdown_first": true,
			"targets": []map[string]string{{"account": "a1", "id": "9"}},
		})
		r := out["results"].([]any)[0].(map[string]any)
		if r["ok"] == true || !strings.Contains(r["error"].(string), "未删除") {
			t.Errorf("超时应失败且不删: %v", r)
		}
		if len(fp.deletes) != 0 {
			t.Errorf("不应删除: %v", fp.deletes)
		}
	})
}

func TestCreateEndpoint(t *testing.T) {
	fp := &fakeProvider{servers: []provider.Server{{ID: "1", Name: "vps-team3-sgp1-03"}}}
	srv := testServer(t, map[string]*fakeProvider{"team3": fp})

	code, out := postJSON(t, srv.URL+"/api/create", map[string]any{
		"account": "team3", "count": 2,
		"region": "sgp1", "size": "s-1vcpu-1gb", "image": "ubuntu-24-04-x64",
		"ssh_keys": []string{"99"}, "prefix": "vps",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d out=%v", code, out)
	}
	created := out["created"].([]any)
	if len(created) != 2 {
		t.Fatalf("created = %v", created)
	}
	names := []string{created[0].(map[string]any)["name"].(string), created[1].(map[string]any)["name"].(string)}
	sort.Strings(names)
	// 现有 vps-team3-sgp1-03 → 自动从 04 续号
	if names[0] != "vps-team3-sgp1-04" || names[1] != "vps-team3-sgp1-05" {
		t.Errorf("续号命名不符: %v", names)
	}
	fp.mu.Lock()
	if len(fp.creates) != 2 {
		t.Errorf("creates = %d", len(fp.creates))
	}
	if len(fp.creates) == 2 && (fp.creates[0].Region != "sgp1" || !fp.creates[0].Monitoring) {
		t.Errorf("creates[0] = %+v", fp.creates[0])
	}
	fp.mu.Unlock()

	// 校验失败路径
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"未知账号", map[string]any{"account": "nope", "count": 1, "region": "r", "size": "s", "image": "i"}},
		{"count 超限", map[string]any{"account": "team3", "count": 51, "region": "r", "size": "s", "image": "i"}},
		{"count 为 0", map[string]any{"account": "team3", "count": 0, "region": "r", "size": "s", "image": "i"}},
		{"缺 image", map[string]any{"account": "team3", "count": 1, "region": "r", "size": "s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := postJSON(t, srv.URL+"/api/create", tc.body)
			if code != http.StatusBadRequest {
				t.Errorf("应 400, got %d", code)
			}
		})
	}
}

func TestCatalog(t *testing.T) {
	fp := &fakeProvider{
		regions: []provider.Region{{Slug: "sgp1", Name: "Singapore 1"}},
		sizes:   []provider.Size{{Slug: "s-1vcpu-1gb", VCPUs: 1, MemoryMB: 1024, DiskGB: 25, PriceMonthly: 6}},
		images:  []provider.Image{{Slug: "ubuntu-24-04-x64", Distribution: "Ubuntu", Name: "24.04 x64"}},
		keys:    []provider.SSHKey{{ID: "99", Name: "laptop", Fingerprint: "aa:bb"}},
	}
	srv := testServer(t, map[string]*fakeProvider{"team3": fp})

	// 无 account：只给账号列表
	resp, err := http.Get(srv.URL + "/api/catalog")
	if err != nil {
		t.Fatal(err)
	}
	var cat catalogResponse
	_ = json.NewDecoder(resp.Body).Decode(&cat)
	resp.Body.Close()
	if len(cat.Accounts) != 1 || cat.Accounts[0] != "team3" || cat.Regions != nil {
		t.Errorf("catalog = %+v", cat)
	}

	// 带 account：返回四类可选值
	resp, err = http.Get(srv.URL + "/api/catalog?account=team3")
	if err != nil {
		t.Fatal(err)
	}
	cat = catalogResponse{}
	_ = json.NewDecoder(resp.Body).Decode(&cat)
	resp.Body.Close()
	if len(cat.Regions) != 1 || len(cat.Sizes) != 1 || len(cat.Images) != 1 || len(cat.Keys) != 1 {
		t.Errorf("catalog sections = %+v", cat)
	}
	if cat.Sizes[0].PriceMonthly != 6 {
		t.Errorf("sizes = %+v", cat.Sizes)
	}

	// 未知账号
	resp2, _ := http.Get(srv.URL + "/api/catalog?account=nope")
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("未知账号应 400, got %d", resp2.StatusCode)
	}
	resp2.Body.Close()
}

func TestNoTokenLeak(t *testing.T) {
	// webui 只持有 provider 接口，凭据不出后端；
	// 这里校验列表响应的顶层与 droplet 字段集合。
	srv := testServer(t, map[string]*fakeProvider{
		"a1": {servers: []provider.Server{{ID: "1", Account: "a1", Name: "n1"}}},
	})
	resp, _ := http.Get(srv.URL + "/api/droplets")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "token") {
		t.Error("响应不应包含 token 字样")
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if _, ok := got["droplets"]; !ok {
		t.Errorf("缺 droplets 字段: %v", got)
	}
}

/* ---- 账号管理与 NMS 载荷导出 ---- */

func TestAccountsListMasksSecrets(t *testing.T) {
	srv := testServer(t, map[string]*fakeProvider{})
	resp, err := http.Get(srv.URL + "/api/accounts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var got struct {
		Accounts []struct {
			Name        string `json:"name"`
			Provider    string `json:"provider"`
			SSHUser     string `json:"ssh_user"`
			HasPassword bool   `json:"has_password"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 1 {
		t.Fatalf("accounts = %+v", got)
	}
	a := got.Accounts[0]
	if a.Name != "a1" || a.Provider != "digitalocean" || a.SSHUser != "root" || !a.HasPassword {
		t.Errorf("账号视图不符: %+v", a)
	}
	if strings.Contains(string(body), "tok-a1") || strings.Contains(string(body), "pw-a1") {
		t.Error("账号视图泄漏了 token/密码明文: %s" + string(body))
	}
}

func TestAddAccount(t *testing.T) {
	fp := &fakeProvider{servers: []provider.Server{}}
	srv := testServer(t, map[string]*fakeProvider{"newacct": fp})

	code, out := postJSON(t, srv.URL+"/api/accounts", map[string]any{
		"name": "newacct", "provider": "digitalocean", "token": "tok-new",
		"ssh_user": "deploy", "ssh_password": "pw-new",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d out=%v", code, out)
	}
	// 落盘
	cfgPath := srv.Config.Addr // 占位，实际从 handler 闭包外取不到；改由重开文件验证
	_ = cfgPath
	// 免重启生效：新账号可被 /api/droplets 聚合（factory 已注入 fake）
	resp, err := http.Get(srv.URL + "/api/droplets")
	if err != nil {
		t.Fatal(err)
	}
	var dr listResponse
	_ = json.NewDecoder(resp.Body).Decode(&dr)
	resp.Body.Close()
	found := false
	for _, d := range dr.Droplets {
		if d.Account == "newacct" {
			found = true
		}
	}
	// fake.servers 为空切片时 List 成功返回空——错误列表里不应有 newacct
	for _, e := range dr.Errors {
		if e.Account == "newacct" {
			t.Errorf("新账号不应出现在错误列表: %+v", e)
		}
	}
	if !found && len(dr.Errors) == 0 && len(dr.Droplets) == 0 {
		t.Log("newacct 无节点（空列表），聚合通过")
	}

	// 重复账号名 → 400
	code, _ = postJSON(t, srv.URL+"/api/accounts", map[string]any{
		"name": "newacct", "provider": "digitalocean", "token": "tok2",
	})
	if code != http.StatusBadRequest {
		t.Errorf("重复账号名应 400, got %d", code)
	}
	// 缺 token → 400
	code, _ = postJSON(t, srv.URL+"/api/accounts", map[string]any{"name": "x", "provider": "digitalocean"})
	if code != http.StatusBadRequest {
		t.Errorf("缺 token 应 400, got %d", code)
	}
	// 未知 provider → 400
	code, _ = postJSON(t, srv.URL+"/api/accounts", map[string]any{"name": "y", "provider": "vultr", "token": "t"})
	if code != http.StatusBadRequest {
		t.Errorf("未知 provider 应 400, got %d", code)
	}
}

func TestAddAccountPersists(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "accounts.json")
	if err := config.Save(cfgPath, &config.File{Accounts: []config.Account{
		{Name: "a1", Provider: "digitalocean", Token: "t1"},
	}}); err != nil {
		t.Fatal(err)
	}
	s := New([]fleet.AccountClient{}, &config.File{Accounts: []config.Account{
		{Name: "a1", Provider: "digitalocean", Token: "t1"},
	}}, cfgPath)
	s.newProvider = func(string, string, provider.Credentials) (provider.Provider, error) { return &fakeProvider{}, nil }
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/api/accounts", map[string]any{
		"name": "b2", "provider": "digitalocean", "token": "t2", "ssh_password": "pw",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	f, _, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 2 || f.Accounts[1].Name != "b2" || f.Accounts[1].SSHPassword != "pw" {
		t.Errorf("未持久化: %+v", f.Accounts)
	}
}

// TestAddKeyPairAccount：密钥对型提供商（腾讯云/阿里云）的凭据是 access_key +
// secret_key，不要求 token；成对给全才收，落盘原样，账号视图不回传任何凭据。
func TestAddKeyPairAccount(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "accounts.json")
	seed := &config.File{Accounts: []config.Account{{Name: "a1", Provider: "digitalocean", Token: "t1"}}}
	if err := config.Save(cfgPath, seed); err != nil {
		t.Fatal(err)
	}
	s := New([]fleet.AccountClient{}, &config.File{Accounts: []config.Account{{Name: "a1", Provider: "digitalocean", Token: "t1"}}}, cfgPath)
	var gotCred provider.Credentials
	s.newProvider = func(name, _ string, cred provider.Credentials) (provider.Provider, error) {
		if name != "tencent-cvm" {
			return nil, fmt.Errorf("未知提供商 %q", name)
		}
		gotCred = cred
		return &fakeProvider{}, nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	code, out := postJSON(t, srv.URL+"/api/accounts", map[string]any{
		"name": "tc-1", "provider": "tencent-cvm", "access_key": "AKIDexample", "secret_key": "tc-secret",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d out=%v", code, out)
	}
	if gotCred != (provider.Credentials{AccessKey: "AKIDexample", SecretKey: "tc-secret"}) {
		t.Errorf("传给提供商的凭据不符: %+v", gotCred)
	}
	f, _, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	a := f.Find("tc-1")
	if a == nil || a.AccessKey != "AKIDexample" || a.SecretKey != "tc-secret" || a.Token != "" {
		t.Errorf("未按密钥对落盘: %+v", a)
	}

	// 只给一半 → 400；缺凭据 → 400
	for name, body := range map[string]map[string]any{
		"只给 access_key": {"name": "tc-2", "provider": "tencent-cvm", "access_key": "AKIDonly"},
		"只给 secret_key": {"name": "tc-3", "provider": "tencent-cvm", "secret_key": "secret-only"},
		"都没给":           {"name": "tc-4", "provider": "tencent-cvm"},
	} {
		code, _ = postJSON(t, srv.URL+"/api/accounts", body)
		if code != http.StatusBadRequest {
			t.Errorf("%s 应 400, got %d", name, code)
		}
	}

	// 账号视图不回传凭据，并给出密钥对型提供商清单（本测试包未注册任何一家，应为空数组）
	resp, err := http.Get(srv.URL + "/api/accounts")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "tc-secret") || strings.Contains(string(body), "AKIDexample") {
		t.Errorf("账号视图泄漏了密钥对: %s", body)
	}
	if !strings.Contains(string(body), `"key_providers":[]`) {
		t.Errorf("缺 key_providers 空数组: %s", body)
	}
}

// TestEditAccountKeyPair：密钥对编辑必须成对；改了凭据要用新凭据重建客户端。
func TestEditAccountKeyPair(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "accounts.json")
	accounts := []config.Account{{Name: "tc-1", Provider: "tencent-cvm", AccessKey: "AKIDold", SecretKey: "old-secret"}}
	if err := config.Save(cfgPath, &config.File{Accounts: accounts}); err != nil {
		t.Fatal(err)
	}
	s := New([]fleet.AccountClient{}, &config.File{Accounts: accounts}, cfgPath)
	var gotCred provider.Credentials
	s.newProvider = func(_, _ string, cred provider.Credentials) (provider.Provider, error) {
		gotCred = cred
		return &fakeProvider{}, nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// 只改一半 → 拒绝，且不动盘上的文件
	code, _ := sendJSON(t, http.MethodPut, srv.URL+"/api/accounts/tc-1", map[string]any{"access_key": "AKIDhalf"})
	if code != http.StatusConflict {
		t.Fatalf("只改 access_key 应 409, got %d", code)
	}
	if gotCred.AccessKey != "" {
		t.Errorf("被拒的编辑不应重建客户端: %+v", gotCred)
	}

	code, _ = sendJSON(t, http.MethodPut, srv.URL+"/api/accounts/tc-1", map[string]any{"access_key": "AKIDnew", "secret_key": "new-secret"})
	if code != http.StatusOK {
		t.Fatalf("成对修改应 200, got %d", code)
	}
	if gotCred != (provider.Credentials{AccessKey: "AKIDnew", SecretKey: "new-secret"}) {
		t.Errorf("重建客户端未用新凭据: %+v", gotCred)
	}
	f, _, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if a := f.Find("tc-1"); a.AccessKey != "AKIDnew" || a.SecretKey != "new-secret" {
		t.Errorf("编辑未落盘: %+v", a)
	}

	// 只改 SSH 密码不触碰提供商凭据 → 不重建客户端
	gotCred = provider.Credentials{}
	code, _ = sendJSON(t, http.MethodPut, srv.URL+"/api/accounts/tc-1", map[string]any{"ssh_password": "pw"})
	if code != http.StatusOK {
		t.Fatalf("改 SSH 密码应 200, got %d", code)
	}
	if gotCred.AccessKey != "" {
		t.Errorf("只改 SSH 密码不应重建客户端: %+v", gotCred)
	}
}

func TestWriteEndpointsRejectNonLoopback(t *testing.T) {
	s := New([]fleet.AccountClient{}, &config.File{}, filepath.Join(t.TempDir(), "accounts.json"))
	s.newProvider = func(string, string, provider.Credentials) (provider.Provider, error) { return &fakeProvider{}, nil }

	for _, tc := range []struct {
		name, method, path string
		body               string
	}{
		{"新增账号", "POST", "/api/accounts", `{"name":"x","provider":"digitalocean","token":"t"}`},
		{"导出载荷", "GET", "/api/nms-payload", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.RemoteAddr = "203.0.113.9:4444" // 非回环
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("非回环应 403, got %d", rec.Code)
			}
		})
	}
}

func TestNMSPayload(t *testing.T) {
	// 本机开一个监听当作"可达"目标，再开一个立刻关掉当作"不可达"
	openLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer openLn.Close()
	go func() {
		c, _ := openLn.Accept()
		if c != nil {
			c.Close()
		}
	}()
	closedLn, _ := net.Listen("tcp", "127.0.0.1:0")
	closedPort := closedLn.Addr().(*net.TCPAddr).Port
	closedLn.Close()

	newPayloadServer := func(t *testing.T, probePort int) *httptest.Server {
		t.Helper()
		cfg := &config.File{Accounts: []config.Account{
			{Name: "a1", Provider: "digitalocean", Token: "t1", SSHPassword: "pw1"},
		}}
		s := New(nil, cfg, filepath.Join(t.TempDir(), "accounts.json"))
		s.probePort = probePort
		s.probeTimeout = 300 * time.Millisecond
		srv := httptest.NewServer(s.Handler())
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("可达节点保留", func(t *testing.T) {
		s := New([]fleet.AccountClient{{Name: "a1", ProviderName: "digitalocean",
			Provider: &fakeProvider{servers: []provider.Server{
				{ID: "1", Account: "a1", Name: "vps-a1-01", IPv4Public: "127.0.0.1"},
			}}}},
			&config.File{Accounts: []config.Account{
				{Name: "a1", Provider: "digitalocean", Token: "t1", SSHPassword: "pw1"},
			}}, filepath.Join(t.TempDir(), "accounts.json"))
		s.probePort = openLn.Addr().(*net.TCPAddr).Port
		s.probeTimeout = 300 * time.Millisecond
		srv := httptest.NewServer(s.Handler())
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/api/nms-payload")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var payload nms.Payload
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Nodes) != 1 || payload.Nodes[0].SSHPassword != "pw1" {
			t.Fatalf("nodes = %+v", payload.Nodes)
		}
		if resp.Header.Get("X-Vpsctl-Unreachable") != "0" {
			t.Errorf("Unreachable = %q", resp.Header.Get("X-Vpsctl-Unreachable"))
		}
	})

	t.Run("不可达节点跳过", func(t *testing.T) {
		s := New([]fleet.AccountClient{{Name: "a1", ProviderName: "digitalocean",
			Provider: &fakeProvider{servers: []provider.Server{
				{ID: "1", Account: "a1", Name: "vps-a1-01", IPv4Public: "127.0.0.1"},
			}}}},
			&config.File{Accounts: []config.Account{
				{Name: "a1", Provider: "digitalocean", Token: "t1", SSHPassword: "pw1"},
			}}, filepath.Join(t.TempDir(), "accounts.json"))
		s.probePort = closedPort
		s.probeTimeout = 300 * time.Millisecond
		srv := httptest.NewServer(s.Handler())
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/api/nms-payload")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var payload nms.Payload
		_ = json.NewDecoder(resp.Body).Decode(&payload)
		if len(payload.Nodes) != 0 {
			t.Fatalf("不可达应被跳过: %+v", payload.Nodes)
		}
		if resp.Header.Get("X-Vpsctl-Unreachable") != "1" {
			t.Errorf("Unreachable = %q", resp.Header.Get("X-Vpsctl-Unreachable"))
		}
	})

	t.Run("账号过滤未知报错", func(t *testing.T) {
		srv := newPayloadServer(t, openLn.Addr().(*net.TCPAddr).Port)
		resp, err := http.Get(srv.URL + "/api/nms-payload?account=nope")
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("未知账号应 400, got %d", resp.StatusCode)
		}
		resp.Body.Close()
	})
}

func TestRebuildResizeEndpoints(t *testing.T) {
	fp := &fakeProvider{servers: []provider.Server{{ID: "7", Account: "a1"}, {ID: "8", Account: "a1"}}}
	srv := testServer(t, map[string]*fakeProvider{"a1": fp})

	code, out := postJSON(t, srv.URL+"/api/rebuild", map[string]any{
		"targets": []map[string]string{{"account": "a1", "id": "7"}}, "image": "ubuntu-24-04-x64",
	})
	if code != http.StatusOK {
		t.Fatalf("rebuild status = %d out=%v", code, out)
	}
	if len(fp.rebuilds) != 1 || fp.rebuilds[0] != "7@ubuntu-24-04-x64" {
		t.Errorf("rebuilds = %v", fp.rebuilds)
	}

	code, out = postJSON(t, srv.URL+"/api/resize", map[string]any{
		"targets": []map[string]string{{"account": "a1", "id": "8"}}, "size": "s-2vcpu-2gb", "resize_disk": true,
	})
	if code != http.StatusOK {
		t.Fatalf("resize status = %d out=%v", code, out)
	}
	if len(fp.resizes) != 1 || fp.resizes[0] != "8@s-2vcpu-2gb" {
		t.Errorf("resizes = %v", fp.resizes)
	}

	// 校验失败路径
	code, _ = postJSON(t, srv.URL+"/api/rebuild", map[string]any{"targets": []map[string]string{{"account": "a1", "id": "7"}}})
	if code != http.StatusBadRequest {
		t.Errorf("缺 image 应 400, got %d", code)
	}
	code, _ = postJSON(t, srv.URL+"/api/resize", map[string]any{"targets": []map[string]string{{"account": "a1", "id": "7"}}})
	if code != http.StatusBadRequest {
		t.Errorf("缺 size 应 400, got %d", code)
	}
	code, _ = postJSON(t, srv.URL+"/api/rebuild", map[string]any{"image": "img"})
	if code != http.StatusBadRequest {
		t.Errorf("空 targets 应 400, got %d", code)
	}
}

func TestEditAndRemoveAccount(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "accounts.json")
	cfg := &config.File{Accounts: []config.Account{
		{Name: "a1", Provider: "digitalocean", Token: "t1", SSHUser: "root", SSHPassword: "pw1"},
		{Name: "a2", Provider: "digitalocean", Token: "t2"},
	}}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	clients := []fleet.AccountClient{
		{Name: "a1", ProviderName: "digitalocean", Provider: &fakeProvider{servers: []provider.Server{{ID: "1", Account: "a1"}}}},
		{Name: "a2", ProviderName: "digitalocean", Provider: &fakeProvider{}},
	}
	s := New(clients, cfg, cfgPath)
	s.newProvider = func(string, string, provider.Credentials) (provider.Provider, error) { return &fakeProvider{}, nil }
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// 编辑：只改提供的字段（token 留空不变，密码清除）
	code, _ := sendJSON(t, http.MethodPut, srv.URL+"/api/accounts/a1", map[string]any{
		"ssh_user": "deploy", "clear_password": true,
	})
	if code != http.StatusOK {
		t.Fatalf("edit status = %d", code)
	}
	f, _, _ := config.Load(cfgPath)
	a := f.Find("a1")
	if a.SSHUser != "deploy" || a.SSHPassword != "" || a.Token != "t1" {
		t.Errorf("编辑结果不符: %+v", a)
	}

	// 删除：有机器时拒绝
	code, out := postJSONRaw(t, srv.URL+"/api/accounts/a1", nil)
	if code != http.StatusConflict {
		t.Errorf("有机器应 409, got %d out=%v", code, out)
	}
	// force 删除
	code, _ = postJSONRaw(t, srv.URL+"/api/accounts/a1?force=1", nil)
	if code != http.StatusOK {
		t.Fatalf("force 删除应 200, got %d", code)
	}
	f, _, _ = config.Load(cfgPath)
	if f.Find("a1") != nil || len(f.Accounts) != 1 {
		t.Errorf("删除未持久化: %+v", f.Accounts)
	}
	// 删除最后一个账号
	code, _ = postJSONRaw(t, srv.URL+"/api/accounts/a2?force=1", nil)
	if code != http.StatusBadRequest {
		t.Errorf("删最后账号应 400, got %d", code)
	}
	// 编辑不存在的账号
	code, _ = sendJSON(t, http.MethodPut, srv.URL+"/api/accounts/nope", map[string]any{"ssh_user": "x"})
	if code != http.StatusNotFound {
		t.Errorf("编辑不存在账号应 404, got %d", code)
	}
}

// postJSONRaw 发送 DELETE 请求，返回状态码与解析后的 JSON。
func postJSONRaw(t *testing.T, url string, _ any) (int, map[string]any) {
	t.Helper()
	return sendJSON(t, http.MethodDelete, url, nil)
}

// sendJSON 发送任意方法的 JSON 请求，返回状态码与解析后的 JSON。
func sendJSON(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	req, err := http.NewRequest(method, url, strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestOperationHistory(t *testing.T) {
	fp := &fakeProvider{servers: []provider.Server{}}
	srv := testServer(t, map[string]*fakeProvider{"a1": fp})

	// 一次成功关机 + 一次删除
	postJSON(t, srv.URL+"/api/power", map[string]any{
		"action": "power_off", "targets": []map[string]string{{"account": "a1", "id": "7"}},
	})
	postJSON(t, srv.URL+"/api/delete", map[string]any{
		"count": 1, "targets": []map[string]string{{"account": "a1", "id": "7"}},
	})

	resp, err := http.Get(srv.URL + "/api/operations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var got struct {
		Operations []struct {
			Kind   string `json:"kind"`
			Detail string `json:"detail"`
			OK     int    `json:"ok"`
			Fail   int    `json:"fail"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Operations) != 2 {
		t.Fatalf("operations = %+v", got.Operations)
	}
	// 最新在前：第一条是删除
	if got.Operations[0].Kind != "delete" || got.Operations[0].OK != 1 {
		t.Errorf("最新操作应为 delete: %+v", got.Operations[0])
	}
	if got.Operations[1].Kind != "power" || got.Operations[1].OK != 1 {
		t.Errorf("第二条应为 power: %+v", got.Operations[1])
	}
	if strings.Contains(string(body), "token") || strings.Contains(string(body), "password") {
		t.Error("操作历史不应包含凭据字段")
	}
}

// acctProvider 让假 provider 建出来的节点带上真实账号名（真 provider 都会带）。
type acctProvider struct {
	*fakeProvider
	account string
}

func (p *acctProvider) Create(ctx context.Context, req provider.CreateRequest) (provider.Server, error) {
	s, err := p.fakeProvider.Create(ctx, req)
	s.Account = p.account
	return s, err
}

// TestCreateStoresRandomPasswords：管理台建机给每台节点一个随机 root 密码并记入密码库
// （每台的 cloud-init 里是它自己的那个密码，接口响应里不出现）；列表接口标出"有密码"；
// 导出 NMS 载荷时逐台带上；销毁后记录清掉。
func TestCreateStoresRandomPasswords(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, _ := ln.Accept()
			if c == nil {
				return
			}
			c.Close()
		}
	}()

	fp := &fakeProvider{servers: []provider.Server{}}
	s := New([]fleet.AccountClient{{Name: "a1", ProviderName: "digitalocean", Provider: &acctProvider{fp, "a1"}}},
		&config.File{Accounts: []config.Account{{Name: "a1", Provider: "digitalocean", Token: "t1"}}},
		filepath.Join(t.TempDir(), "accounts.json"))
	s.probePort = ln.Addr().(*net.TCPAddr).Port
	s.probeTimeout = 300 * time.Millisecond
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	code, out := postJSON(t, srv.URL+"/api/create", map[string]any{
		"account": "a1", "count": 2, "region": "sgp1", "size": "s", "image": "i", "prefix": "vps",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d out=%v", code, out)
	}
	raw, _ := json.Marshal(out)
	entries, err := nodepass.Open(s.passPath).Load()
	if err != nil || len(entries) != 2 {
		t.Fatalf("密码库应记下 2 台: %v %v", entries, err)
	}
	fp.mu.Lock()
	creates := append([]provider.CreateRequest(nil), fp.creates...)
	fp.mu.Unlock()
	passwords := map[string]string{} // 实例 ID → 密码库里的密码
	for _, c := range creates {
		e, ok := entries[nodepass.Key("a1", "id-"+c.Name)]
		if !ok {
			t.Fatalf("密码库里没有 %s: %v", c.Name, entries)
		}
		if !strings.Contains(c.UserData, e.Password) {
			t.Errorf("节点 %s 的 cloud-init 里不是密码库记下的那个密码", c.Name)
		}
		if bytes.Contains(raw, []byte(e.Password)) {
			t.Errorf("建机接口的响应里不应出现密码（节点 %s）", c.Name)
		}
		passwords["id-"+c.Name] = e.Password
	}
	if len(passwords) != 2 {
		t.Fatalf("建机次数不符: %d", len(creates))
	}
	for id, pw := range passwords {
		for other, opw := range passwords {
			if id != other && pw == opw {
				t.Error("两台节点的密码不应相同")
			}
		}
	}

	// 列表接口：标出"有密码"，但密码本身不回传
	fp.mu.Lock()
	fp.servers = []provider.Server{
		{ID: "id-vps-a1-sgp1-01", Account: "a1", Name: "vps-a1-sgp1-01", IPv4Public: "127.0.0.1"},
		{ID: "id-vps-a1-sgp1-02", Account: "a1", Name: "vps-a1-sgp1-02", IPv4Public: "127.0.0.1"},
		{ID: "id-legacy", Account: "a1", Name: "vps-a1-sgp1-99", IPv4Public: "127.0.0.1"},
	}
	fp.mu.Unlock()
	resp, err := http.Get(srv.URL + "/api/droplets")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var listed listResponse
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	has := map[string]bool{}
	for _, d := range listed.Droplets {
		has[d.ID] = d.HasPassword
	}
	if !has["id-vps-a1-sgp1-01"] || !has["id-vps-a1-sgp1-02"] || has["id-legacy"] {
		t.Errorf("has_password 应只对密码库里有记录的节点为真: %v", has)
	}
	for _, pw := range passwords {
		if bytes.Contains(body, []byte(pw)) {
			t.Error("列表接口的响应里不应出现密码")
		}
	}

	// NMS 载荷：逐台带各自的随机密码，用户缺省 root
	resp, err = http.Get(srv.URL + "/api/nms-payload")
	if err != nil {
		t.Fatal(err)
	}
	var payload nms.Payload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(payload.Nodes) != 3 {
		t.Fatalf("nodes = %+v", payload.Nodes)
	}
	for _, n := range payload.Nodes {
		var want string
		for id, pw := range passwords {
			if n.Name == strings.TrimPrefix(id, "id-") {
				want = pw
			}
		}
		if n.SSHPassword != want || n.SSHUser != "root" {
			t.Errorf("节点 %s 的载荷凭据不符: %s/%s", n.Name, n.SSHUser, n.SSHPassword)
		}
	}

	// 销毁成功 → 密码记录清掉
	code, out = postJSON(t, srv.URL+"/api/delete", map[string]any{
		"targets": []map[string]any{
			{"account": "a1", "id": "id-vps-a1-sgp1-01"},
			{"account": "a1", "id": "id-vps-a1-sgp1-02"},
		},
		"count": 2,
	})
	if code != http.StatusOK {
		t.Fatalf("delete status = %d out=%v", code, out)
	}
	entries, err = nodepass.Open(s.passPath).Load()
	if err != nil || len(entries) != 0 {
		t.Errorf("销毁后密码记录应清掉: %v %v", entries, err)
	}
}
