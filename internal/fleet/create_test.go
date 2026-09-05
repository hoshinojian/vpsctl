package fleet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
)

// fakeProvider 只实现编排用到的 Create/Get，其余方法不该被调用。
type fakeProvider struct {
	mu        sync.Mutex
	createErr map[string]error             // 按名称注入创建失败
	getSeq    map[string][]provider.Server // 按 ID 依次返回的 Get 序列
	creates   []provider.CreateRequest
}

func (f *fakeProvider) Create(_ context.Context, req provider.CreateRequest) (provider.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, req)
	if err := f.createErr[req.Name]; err != nil {
		return provider.Server{}, err
	}
	return provider.Server{ID: "id-" + req.Name, Name: req.Name, Status: "new", Account: "x"}, nil
}

func (f *fakeProvider) Get(_ context.Context, id string) (provider.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seq := f.getSeq[id]
	if len(seq) == 0 {
		return provider.Server{}, errors.New("no seq for " + id)
	}
	s := seq[0]
	f.getSeq[id] = seq[1:]
	return s, nil
}

func (f *fakeProvider) List(context.Context) ([]provider.Server, error) {
	return nil, errors.New("unexpected")
}
func (f *fakeProvider) Delete(context.Context, string) error { return errors.New("unexpected") }
func (f *fakeProvider) Power(context.Context, string, string) (provider.ActionRef, error) {
	return provider.ActionRef{}, errors.New("unexpected")
}
func (f *fakeProvider) ActionStatus(context.Context, provider.ActionRef) (string, error) {
	return "", errors.New("unexpected")
}
func (f *fakeProvider) SSHKeys(context.Context) ([]provider.SSHKey, error) {
	return nil, errors.New("unexpected")
}
func (f *fakeProvider) Regions(context.Context) ([]provider.Region, error) {
	return nil, errors.New("unexpected")
}
func (f *fakeProvider) Sizes(context.Context) ([]provider.Size, error) {
	return nil, errors.New("unexpected")
}
func (f *fakeProvider) Images(context.Context) ([]provider.Image, error) {
	return nil, errors.New("unexpected")
}

func opts(clients ...AccountClient) Options {
	return Options{
		Clients: clients, Count: 2, Prefix: "vps",
		Region: "sgp1", Size: "s-1vcpu-1gb", Image: "ubuntu-24-04-x64",
	}
}

func TestSelectClients(t *testing.T) {
	clients := []AccountClient{
		{Name: "team2"}, {Name: "team3"}, {Name: "team4"},
	}

	got, err := SelectClients(clients, nil)
	if err != nil || len(got) != 3 {
		t.Fatalf("空 only 应返回全部: %v %v", got, err)
	}

	got, err = SelectClients(clients, []string{"team3"})
	if err != nil || len(got) != 1 || got[0].Name != "team3" {
		t.Fatalf("单账号过滤不符: %v %v", got, err)
	}

	got, err = SelectClients(clients, []string{"team4", "team2"})
	if err != nil || len(got) != 2 || got[0].Name != "team4" || got[1].Name != "team2" {
		t.Fatalf("多账号过滤不符: %v %v", got, err)
	}

	_, err = SelectClients(clients, []string{"team9"})
	if err == nil || !strings.Contains(err.Error(), "team9") || !strings.Contains(err.Error(), "team2") {
		t.Fatalf("未知账号应报错并列出可用名: %v", err)
	}
}

func TestNames(t *testing.T) {
	got := Names("vps", "do-1", 1, 3)
	want := []string{"vps-do-1-01", "vps-do-1-02", "vps-do-1-03"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names = %v, want %v", got, want)
		}
	}
	got = Names("vps", "do-1", 12, 2)
	if got[0] != "vps-do-1-12" || got[1] != "vps-do-1-13" {
		t.Errorf("StartIndex 偏移不符: %v", got)
	}
}

func TestPlan(t *testing.T) {
	plan, err := Plan(opts(
		AccountClient{Name: "a1", ProviderName: "digitalocean", Provider: &fakeProvider{}},
		AccountClient{Name: "a2", ProviderName: "digitalocean", Provider: &fakeProvider{}},
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 || plan[0].Account != "a1" || plan[1].Account != "a2" {
		t.Fatalf("plan = %+v", plan)
	}
	if plan[0].Provider != "digitalocean" || plan[0].Count != 2 {
		t.Errorf("entry = %+v", plan[0])
	}
	if plan[0].Names[0] != "vps-a1-01" || plan[1].Names[1] != "vps-a2-02" {
		t.Errorf("计划命名不符: %+v", plan)
	}
}

func TestPlanValidate(t *testing.T) {
	o := opts(AccountClient{Name: "a1", Provider: &fakeProvider{}})
	o.Count = 0
	if _, err := Plan(o); err == nil {
		t.Error("count=0 应报错")
	}
	o = opts(AccountClient{Name: "a1", Provider: &fakeProvider{}})
	o.Region = ""
	if _, err := Plan(o); err == nil || !strings.Contains(err.Error(), "region") {
		t.Errorf("缺 region 应报错: %v", err)
	}
}

func TestCreateSuccess(t *testing.T) {
	now := time.Date(2026, 9, 4, 15, 30, 0, 0, time.UTC)
	o := opts(
		AccountClient{Name: "a1", ProviderName: "digitalocean", Provider: &fakeProvider{}},
		AccountClient{Name: "a2", ProviderName: "digitalocean", Provider: &fakeProvider{}},
	)
	o.Now = func() time.Time { return now }

	res, err := Create(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Batch != "20260904T153000Z" {
		t.Errorf("batch = %q", res.Batch)
	}
	if len(res.Created) != 4 || len(res.Errors) != 0 {
		t.Fatalf("created=%d errors=%d", len(res.Created), len(res.Errors))
	}
	if res.Requested["a1"] != 2 || res.Requested["a2"] != 2 {
		t.Errorf("requested = %+v", res.Requested)
	}
	// 排序后前两条是 a1
	if res.Created[0].Name != "vps-a1-01" || res.Created[1].Name != "vps-a1-02" ||
		res.Created[2].Name != "vps-a2-01" {
		t.Errorf("命名/排序不符: %+v", res.Created)
	}
	if res.Created[0].ID != "id-vps-a1-01" {
		t.Errorf("ID 不符: %+v", res.Created[0])
	}
}

func TestCreateTagsAndRequest(t *testing.T) {
	fp := &fakeProvider{}
	o := opts(AccountClient{Name: "a1", Provider: fp})
	o.Monitoring = true
	o.ExtraTags = []string{"env:lab"}
	o.Now = func() time.Time { return time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC) }

	if _, err := Create(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if len(fp.creates) != 2 {
		t.Fatalf("creates = %d", len(fp.creates))
	}
	for _, req := range fp.creates {
		if req.Region != "sgp1" || req.Size != "s-1vcpu-1gb" || req.Image != "ubuntu-24-04-x64" || !req.Monitoring {
			t.Errorf("请求不符: %+v", req)
		}
		wantTags := []string{"env:lab", "batch:20260904T000000Z"}
		if len(req.Tags) != 2 || req.Tags[0] != wantTags[0] || req.Tags[1] != wantTags[1] {
			t.Errorf("tags = %v, want %v", req.Tags, wantTags)
		}
	}
}

func TestCreatePartialFailure(t *testing.T) {
	fp := &fakeProvider{createErr: map[string]error{"vps-a1-02": errors.New("429 rate limited")}}
	o := opts(AccountClient{Name: "a1", Provider: fp})

	res, err := Create(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 || res.Created[0].Name != "vps-a1-01" {
		t.Errorf("created = %+v", res.Created)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %+v", res.Errors)
	}
	e := res.Errors[0]
	if e.Account != "a1" || e.Name != "vps-a1-02" || e.Index != 2 || !strings.Contains(e.Error, "429") {
		t.Errorf("error 条目不符: %+v", e)
	}
}

func TestCreateWaitActive(t *testing.T) {
	fp := &fakeProvider{getSeq: map[string][]provider.Server{
		"id-vps-a1-01": {
			{ID: "id-vps-a1-01", Name: "vps-a1-01", Status: "new"},
			{ID: "id-vps-a1-01", Name: "vps-a1-01", Status: "active", IPv4Public: "203.0.113.9"},
		},
	}}
	o := opts(AccountClient{Name: "a1", Provider: fp})
	o.Count = 1
	o.WaitTimeout = 2 * time.Second
	o.PollEvery = time.Millisecond

	res, err := Create(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors = %+v", res.Errors)
	}
	s := res.Created[0]
	if s.Status != "active" || s.IPv4Public != "203.0.113.9" {
		t.Errorf("未等到 active: %+v", s)
	}
}

func TestCreateWaitTimeoutKeepsNode(t *testing.T) {
	fp := &fakeProvider{getSeq: map[string][]provider.Server{
		"id-vps-a1-01": {
			{ID: "id-vps-a1-01", Name: "vps-a1-01", Status: "new"},
			{ID: "id-vps-a1-01", Name: "vps-a1-01", Status: "new"},
			{ID: "id-vps-a1-01", Name: "vps-a1-01", Status: "new"},
		},
	}}
	o := opts(AccountClient{Name: "a1", Provider: fp})
	o.Count = 1
	o.WaitTimeout = 50 * time.Millisecond
	o.PollEvery = time.Millisecond

	res, err := Create(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	// 节点已真实存在：created 与 errors 都要有
	if len(res.Created) != 1 || res.Created[0].Status != "new" {
		t.Errorf("created = %+v", res.Created)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error, "已创建") {
		t.Errorf("errors = %+v", res.Errors)
	}
}
