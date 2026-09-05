package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoshinojian/vpsctl/internal/fleet"
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

func testServer(t *testing.T, fps map[string]*fakeProvider) *httptest.Server {
	t.Helper()
	clients := make([]fleet.AccountClient, 0, len(fps))
	for name, fp := range fps {
		clients = append(clients, fleet.AccountClient{Name: name, ProviderName: "digitalocean", Provider: fp})
	}
	s := New(clients)
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
		s2 := New([]fleet.AccountClient{{Name: "a1", ProviderName: "digitalocean", Provider: fp}})
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
	fp := &fakeProvider{servers: []provider.Server{{ID: "1", Name: "vps-team3-03"}}}
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
	// 现有 vps-team3-03 → 自动从 04 续号
	if names[0] != "vps-team3-04" || names[1] != "vps-team3-05" {
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
