package main

// 置备规则单测：缺省给每台节点一个随机 root 密码；只有显式不注入密码（--user-data /
// --no-password）时才可能建出裸机，这时裸机保护（P81）与"账号固定密码不得被导出时
// 误用"的互斥规则照旧生效。

import (
	"context"
	"errors"
	"flag"
	"slices"
	"strings"
	"testing"

	"github.com/hoshinojian/vpsctl/internal/config"
	"github.com/hoshinojian/vpsctl/internal/fleet"
	"github.com/hoshinojian/vpsctl/internal/provider"
)

func cfgOf(accts ...config.Account) *config.File {
	return &config.File{Accounts: accts}
}

func clientsOf(names ...string) []fleet.AccountClient {
	out := make([]fleet.AccountClient, 0, len(names))
	for _, n := range names {
		out = append(out, fleet.AccountClient{Name: n})
	}
	return out
}

func pp(accts []config.Account, names ...string) provisionParams {
	return provisionParams{cfg: cfgOf(accts...), clients: clientsOf(names...)}
}

var nodeNames = []string{"vps-a1-sgp1-01", "vps-a2-sgp1-01"}

// 缺省：每个账号的每台节点各拿一个随机密码，写进它自己的 cloud-init；账号配没配
// ssh_password 都一样，配了也不用那个固定密码。
func TestProvisionDefaultIsRandomPerNode(t *testing.T) {
	p := pp([]config.Account{{Name: "a1"}, {Name: "a2", SSHPassword: "fixed-pw"}}, "a1", "a2")
	opts := &fleet.Options{Clients: p.clients, Prefix: "vps", Region: "sgp1", Count: 1}
	generated, err := injectPasswords(p, opts)
	if err != nil {
		t.Fatalf("缺省不应报错：%v", err)
	}
	if len(generated) != 2 || len(opts.UserDataByNode) != 2 || len(opts.UserDataByAccount) != 0 {
		t.Fatalf("应为 2 台节点各生成一份: generated=%v byNode=%v byAccount=%v", generated, opts.UserDataByNode, opts.UserDataByAccount)
	}
	for _, name := range nodeNames {
		e := generated[name]
		if len(e.Password) != 24 || e.User != "root" {
			t.Errorf("节点 %s 的凭据不符: %+v", name, e)
		}
		ud := opts.UserDataByNode[name]
		if !strings.Contains(ud, "password: '"+e.Password+"'") {
			t.Errorf("节点 %s 的 cloud-init 里不是它自己的密码:\n%s", name, ud)
		}
		if strings.Contains(ud, "fixed-pw") {
			t.Errorf("账号的固定密码不应再注入新节点:\n%s", ud)
		}
	}
	if generated[nodeNames[0]].Password == generated[nodeNames[1]].Password {
		t.Error("两台节点的密码不应相同")
	}
}

// 密码写法跟随提供商：公共镜像的 cloud-init 较老的提供商同时下发 list 写法。
func TestProvisionChpasswdFollowsProvider(t *testing.T) {
	p := pp([]config.Account{{Name: "a1", Provider: "tencent-cvm"}}, "a1")
	opts := &fleet.Options{Clients: p.clients, Prefix: "vps", Region: "ap-beijing", Count: 1}
	generated, err := injectPasswords(p, opts)
	if err != nil {
		t.Fatal(err)
	}
	name := "vps-a1-ap-beijing-01"
	if ud := opts.UserDataByNode[name]; !strings.Contains(ud, "list: |\n    root:"+generated[name].Password+"\n") {
		t.Errorf("tencent-cvm 应同时下发 list 写法:\n%s", ud)
	}
	p = pp([]config.Account{{Name: "a1", Provider: "digitalocean"}}, "a1")
	opts = &fleet.Options{Clients: p.clients, Prefix: "vps", Region: "sgp1", Count: 1}
	if _, err := injectPasswords(p, opts); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(opts.UserDataByNode["vps-a1-sgp1-01"], "list:") {
		t.Error("digitalocean 的生成结果不应出现 list 写法")
	}
}

// --no-password：不注入密码 ∧ 无 user-data ∧ 无 ssh-keys → fail-fast，指认账号与逃生门。
func TestProvisionNoPasswordBareFailsFast(t *testing.T) {
	p := pp([]config.Account{{Name: "a1"}}, "a1")
	p.noPassword = true
	opts := &fleet.Options{}
	_, err := injectPasswords(p, opts)
	if err == nil || !strings.Contains(err.Error(), "a1") || !strings.Contains(err.Error(), "--allow-bare") {
		t.Fatalf("裸机应 fail-fast 并指认账号与逃生门：%v", err)
	}
	p.allowBare = true
	if generated, err := injectPasswords(p, opts); err != nil || len(generated) != 0 || len(opts.UserDataByNode) != 0 {
		t.Fatalf("--allow-bare 应放行且什么都不注入：%v %v %v", err, generated, opts.UserDataByNode)
	}
}

// --no-password + --ssh-keys：不算裸机。
func TestProvisionNoPasswordWithSSHKeysNotBare(t *testing.T) {
	p := pp([]config.Account{{Name: "a1"}}, "a1")
	p.noPassword, p.sshKeys = true, []string{"12345"}
	if _, err := injectPasswords(p, &fleet.Options{}); err != nil {
		t.Fatalf("ssh-keys 兜底不应报错：%v", err)
	}
}

// --user-data：原样下发、不注入密码，无密码账号不算裸机。
func TestProvisionUserDataNotBare(t *testing.T) {
	p := pp([]config.Account{{Name: "a1"}}, "a1")
	p.userDataPath = "/tmp/ud.yaml"
	opts := &fleet.Options{}
	generated, err := injectPasswords(p, opts)
	if err != nil || len(generated) != 0 || len(opts.UserDataByNode) != 0 || len(opts.UserDataByAccount) != 0 {
		t.Fatalf("user-data 路径不应生成任何注入：%v %v %v", err, generated, opts.UserDataByNode)
	}
}

// 不注入密码时，配了固定密码的账号必须拦下——否则导出 NMS 载荷会带上机器上没有的密码。
func TestProvisionNoInjectionRejectsFixedPassword(t *testing.T) {
	for _, what := range []string{"--user-data", "--no-password"} {
		p := pp([]config.Account{{Name: "a1"}, {Name: "a2", SSHPassword: "fixed-pw"}}, "a1", "a2")
		if what == "--user-data" {
			p.userDataPath = "/tmp/ud.yaml"
		} else {
			p.noPassword = true
		}
		p.allowBare = true // 裸机放行也不能放过这一条
		_, err := injectPasswords(p, &fleet.Options{})
		if err == nil || !strings.Contains(err.Error(), "互斥") || !strings.Contains(err.Error(), "a2") || !strings.Contains(err.Error(), what) {
			t.Errorf("%s 与账号固定密码应报互斥并指认账号: %v", what, err)
		}
	}
}

// ---- create 序号避让（自愈收口 F 项）：未显式传 -start-index 时盘点各选中
// 账号现有节点，取各账号 NextStartIndex 的 max 作全局起始；显式传参则不盘点。----

// fakeLister 只实现 List 的假 Provider（start-index 盘点用）；其余方法不该被
// 调用——内嵌 nil 接口兜底，误调即 panic。
type fakeLister struct {
	provider.Provider
	servers []provider.Server
	err     error
	calls   int
}

func (f *fakeLister) List(context.Context) ([]provider.Server, error) {
	f.calls++
	return f.servers, f.err
}

func newCreateFS(t *testing.T, args ...string) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.Int("start-index", 1, "")
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return fs
}

// 未显式传：盘点生效，按账号现有节点续号（a1 现有 -05 → 6）。
func TestResolveStartIndexAutoAvoid(t *testing.T) {
	cl := []fleet.AccountClient{
		{Name: "a1", Provider: &fakeLister{servers: []provider.Server{
			{Name: "vps-a1-sgp1-05"},
		}}},
	}
	got, err := resolveStartIndex(context.Background(), cl, newCreateFS(t), 1, "vps", "sgp1")
	if err != nil || got != 6 {
		t.Fatalf("got=%d err=%v, want 6", got, err)
	}
}

// 未显式传：多账号取 max（a1 现有 -05、a2 现有 -10 → 11），避免序号空洞。
func TestResolveStartIndexMultiAccountTakesMax(t *testing.T) {
	cl := []fleet.AccountClient{
		{Name: "a1", Provider: &fakeLister{servers: []provider.Server{
			{Name: "vps-a1-sgp1-05"},
		}}},
		{Name: "a2", Provider: &fakeLister{servers: []provider.Server{
			{Name: "vps-a2-sgp1-10"},
		}}},
	}
	got, err := resolveStartIndex(context.Background(), cl, newCreateFS(t), 1, "vps", "sgp1")
	if err != nil || got != 11 {
		t.Fatalf("got=%d err=%v, want 11", got, err)
	}
}

// 未显式传 + 各账号无同名节点 → 1。
func TestResolveStartIndexNoExisting(t *testing.T) {
	cl := []fleet.AccountClient{{Name: "a1", Provider: &fakeLister{}}}
	got, err := resolveStartIndex(context.Background(), cl, newCreateFS(t), 1, "vps", "sgp1")
	if err != nil || got != 1 {
		t.Fatalf("got=%d err=%v, want 1", got, err)
	}
}

// 显式传：完全跳过盘点（覆盖语义，List 一次都不发），原样返回用户值。
func TestResolveStartIndexExplicitSkipsInventory(t *testing.T) {
	fl := &fakeLister{err: errors.New("盘点不该被触发")}
	cl := []fleet.AccountClient{{Name: "a1", Provider: fl}}
	got, err := resolveStartIndex(context.Background(), cl,
		newCreateFS(t, "-start-index", "3"), 3, "vps", "sgp1")
	if err != nil || got != 3 {
		t.Fatalf("got=%d err=%v, want 3", got, err)
	}
	if fl.calls != 0 {
		t.Errorf("显式传参不应盘点: calls=%d", fl.calls)
	}
}

// 未显式传 + List 失败 → fail-fast 并指认账号（不学 webui 的静默回退）。
func TestResolveStartIndexListFailFast(t *testing.T) {
	cl := []fleet.AccountClient{{Name: "a1", Provider: &fakeLister{err: errors.New("401 Unauthorized")}}}
	_, err := resolveStartIndex(context.Background(), cl, newCreateFS(t), 1, "vps", "sgp1")
	if err == nil || !strings.Contains(err.Error(), "a1") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("List 失败应 fail-fast 并指认账号: %v", err)
	}
}

// fakePrechecker 只实现建机前预检；其余 Provider 方法不该被调用（内嵌 nil 接口兜底）。
type fakePrechecker struct {
	provider.Provider
	err  error
	seen []provider.CreateRequest
}

func (f *fakePrechecker) Precheck(_ context.Context, req provider.CreateRequest) error {
	f.seen = append(f.seen, req)
	return f.err
}

// precheck 只查有预检能力的提供商，查的是将要真发的请求；没有预检能力的如实标为跳过。
func TestPrecheck(t *testing.T) {
	good := &fakePrechecker{}
	clients := []fleet.AccountClient{
		{Name: "a1", Provider: good},
		{Name: "a2", Provider: &fakeLister{}},
	}
	opts := &fleet.Options{Prefix: "vps", Region: "sgp1", Size: "s", Image: "i", Count: 1}
	res, err := precheck(context.Background(), clients, opts, "b1")
	if err != nil {
		t.Fatalf("precheck: %v", err)
	}
	if len(good.seen) != 1 || good.seen[0].Name != "vps-a1-sgp1-01" || !slices.Equal(good.seen[0].Tags, []string{"batch:b1"}) {
		t.Errorf("预检应查将要真发的请求: %+v", good.seen)
	}
	if res["created"] != 0 || !slices.Equal(res["passed"].([]string), []string{"a1"}) || !slices.Equal(res["skipped"].([]string), []string{"a2"}) {
		t.Errorf("预检结果不符: %v", res)
	}

	bad := &fakePrechecker{err: errors.New("库存不足")}
	if _, err := precheck(context.Background(), []fleet.AccountClient{{Name: "a1", Provider: bad}}, opts, "b1"); err == nil ||
		!strings.Contains(err.Error(), "a1") || !strings.Contains(err.Error(), "库存不足") {
		t.Errorf("预检不过应指认账号并带出原因: %v", err)
	}
}
