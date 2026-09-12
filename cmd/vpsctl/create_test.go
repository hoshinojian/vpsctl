package main

// V1 防呆单测（post-campaign-fixes-plan）：裸机判定三条件（无 ssh_password ∧ 无
// --user-data ∧ 无 --ssh-keys）——缺省 fail-fast，--allow-bare 放行；有密码账号
// 与 --user-data 互斥的既有行为不变。

import (
	"strings"
	"testing"

	"github.com/hoshinojian/vpsctl/internal/config"
	"github.com/hoshinojian/vpsctl/internal/fleet"
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
	err := injectPasswords(cfg, clientsOf("a1", "a2"), "", nil, false, &fleet.Options{})
	if err == nil || !strings.Contains(err.Error(), "a1") || !strings.Contains(err.Error(), "--allow-bare") {
		t.Fatalf("裸机应 fail-fast 并指认账号与逃生门：%v", err)
	}
}

// --allow-bare 放行：不报错（警告走 stderr）。
func TestInjectPasswordsBareAllowEscape(t *testing.T) {
	cfg := cfgOf(config.Account{Name: "a1"})
	if err := injectPasswords(cfg, clientsOf("a1"), "", nil, true, &fleet.Options{}); err != nil {
		t.Fatalf("--allow-bare 应放行：%v", err)
	}
}

// 无密码账号 + 传 --user-data（D3 分叉路径）→ 不算裸机，不报错。
func TestInjectPasswordlessWithUserDataNotBare(t *testing.T) {
	cfg := cfgOf(config.Account{Name: "a1"})
	opts := &fleet.Options{}
	if err := injectPasswords(cfg, clientsOf("a1"), "/tmp/ud.yaml", nil, false, opts); err != nil {
		t.Fatalf("user-data 分叉不应报错：%v", err)
	}
	if len(opts.UserDataByAccount) != 0 {
		t.Fatalf("无密码账号不应生成注入 cloud-init：%v", opts.UserDataByAccount)
	}
}

// 无密码账号 + --ssh-keys → 不算裸机。
func TestInjectPasswordlessWithSSHKeysNotBare(t *testing.T) {
	cfg := cfgOf(config.Account{Name: "a1"})
	if err := injectPasswords(cfg, clientsOf("a1"), "", []string{"12345"}, false, &fleet.Options{}); err != nil {
		t.Fatalf("ssh-keys 兜底不应报错：%v", err)
	}
}

// 有密码账号 + --user-data 互斥报错（既有行为回归）。
func TestInjectPasswordUserDataMutuallyExclusive(t *testing.T) {
	cfg := cfgOf(config.Account{Name: "a1", SSHPassword: "pw"})
	err := injectPasswords(cfg, clientsOf("a1"), "/tmp/ud.yaml", nil, false, &fleet.Options{})
	if err == nil || !strings.Contains(err.Error(), "互斥") {
		t.Fatalf("密码账号与 --user-data 同给应报互斥：%v", err)
	}
}
