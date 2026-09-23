package main

// V1 防呆单测（post-campaign-fixes-plan）：裸机判定三条件（无 ssh_password ∧ 无
// --user-data ∧ 无 --ssh-keys）——缺省 fail-fast，--allow-bare 放行；有密码账号
// 与 --user-data 互斥的既有行为不变。

import (
	"context"
	"errors"
	"flag"
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

// 无密码 ∧ 无 user-data ∧ 无 ssh-keys → fail-fast，错误信息含账号与逃生门指引。
func TestInjectPasswordsBareFailsFast(t *testing.T) {
	cfg := cfgOf(config.Account{Name: "a1"}, config.Account{Name: "a2", SSHPassword: "pw"})
	err := injectPasswords(cfg, clientsOf("a1", "a2"), "", nil, 0, false, nil, false, &fleet.Options{})
	if err == nil || !strings.Contains(err.Error(), "a1") || !strings.Contains(err.Error(), "--allow-bare") {
		t.Fatalf("裸机应 fail-fast 并指认账号与逃生门：%v", err)
	}
}

// --allow-bare 放行：不报错（警告走 stderr）。
func TestInjectPasswordsBareAllowEscape(t *testing.T) {
	cfg := cfgOf(config.Account{Name: "a1"})
	if err := injectPasswords(cfg, clientsOf("a1"), "", nil, 0, false, nil, true, &fleet.Options{}); err != nil {
		t.Fatalf("--allow-bare 应放行：%v", err)
	}
}

// 无密码账号 + 传 --user-data（D3 分叉路径）→ 不算裸机，不报错。
func TestInjectPasswordlessWithUserDataNotBare(t *testing.T) {
	cfg := cfgOf(config.Account{Name: "a1"})
	opts := &fleet.Options{}
	if err := injectPasswords(cfg, clientsOf("a1"), "/tmp/ud.yaml", nil, 0, false, nil, false, opts); err != nil {
		t.Fatalf("user-data 分叉不应报错：%v", err)
	}
	if len(opts.UserDataByAccount) != 0 {
		t.Fatalf("无密码账号不应生成注入 cloud-init：%v", opts.UserDataByAccount)
	}
}

// 无密码账号 + --ssh-keys → 不算裸机。
func TestInjectPasswordlessWithSSHKeysNotBare(t *testing.T) {
	cfg := cfgOf(config.Account{Name: "a1"})
	if err := injectPasswords(cfg, clientsOf("a1"), "", []string{"12345"}, 0, false, nil, false, &fleet.Options{}); err != nil {
		t.Fatalf("ssh-keys 兜底不应报错：%v", err)
	}
}

// 有密码账号 + --user-data 互斥报错（既有行为回归）。
func TestInjectPasswordUserDataMutuallyExclusive(t *testing.T) {
	cfg := cfgOf(config.Account{Name: "a1", SSHPassword: "pw"})
	err := injectPasswords(cfg, clientsOf("a1"), "/tmp/ud.yaml", nil, 0, false, nil, false, &fleet.Options{})
	if err == nil || !strings.Contains(err.Error(), "互斥") {
		t.Fatalf("密码账号与 --user-data 同给应报互斥：%v", err)
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
