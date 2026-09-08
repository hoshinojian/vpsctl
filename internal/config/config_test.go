package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAccounts(t *testing.T, content string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	return path
}

const validJSON = `{"accounts":[
	{"name":"do-1","provider":"digitalocean","token":"t1"},
	{"name":"do-2","provider":"digitalocean","token":"t2"}
]}`

func TestLoadOK(t *testing.T) {
	path := writeAccounts(t, validJSON, 0o600)
	f, warns, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("600 权限不应有告警, got %v", warns)
	}
	if len(f.Accounts) != 2 || f.Accounts[0].Name != "do-1" || f.Accounts[1].Token != "t2" {
		t.Errorf("解析结果不符: %+v", f.Accounts)
	}
}

func TestLoadPermWarning(t *testing.T) {
	path := writeAccounts(t, validJSON, 0o644)
	_, warns, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "chmod 600") {
		t.Errorf("644 权限应有 chmod 600 告警, got %v", warns)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantSub string
	}{
		{"坏 JSON", `{"accounts":`, "解析配置"},
		{"空 accounts", `{}`, "accounts 为空"},
		{"缺 token", `{"accounts":[{"name":"a","provider":"digitalocean"}]}`, "缺少 token"},
		{"缺 provider", `{"accounts":[{"name":"a","token":"t"}]}`, "缺少 provider"},
		{"重名", `{"accounts":[{"name":"a","provider":"p","token":"t"},{"name":"a","provider":"p","token":"t"}]}`, "重复"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeAccounts(t, tc.content, 0o600)
			_, _, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("期望错误含 %q, got %v", tc.wantSub, err)
			}
		})
	}
}

func TestLoadSSHCredentials(t *testing.T) {
	path := writeAccounts(t, `{"accounts":[
		{"name":"do-1","provider":"digitalocean","token":"t1",
		 "ssh_user":"deploy","ssh_password":"p@ss'w\"ord"},
		{"name":"do-2","provider":"digitalocean","token":"t2"}
	]}`, 0o600)
	f, _, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.Accounts[0].SSHUser != "deploy" || f.Accounts[0].SSHPassword != `p@ss'w"ord` {
		t.Errorf("凭据未解析: %+v", f.Accounts[0])
	}
	// 可选字段缺省为空，不报错
	if f.Accounts[1].SSHUser != "" || f.Accounts[1].SSHPassword != "" {
		t.Errorf("未配置凭据应为空: %+v", f.Accounts[1])
	}
}

func TestSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	in := &File{Accounts: []Account{
		{Name: "do-1", Provider: "digitalocean", Token: "t1", SSHUser: "root", SSHPassword: "pw"},
		{Name: "do-2", Provider: "digitalocean", Token: "t2"},
	}}
	if err := Save(path, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, warns, err := Load(path)
	if err != nil || len(warns) != 0 {
		t.Fatalf("Load: %v warns=%v", err, warns)
	}
	if len(out.Accounts) != 2 || out.Accounts[0].SSHPassword != "pw" || out.Accounts[1].SSHUser != "" {
		t.Errorf("round-trip 不符: %+v", out.Accounts)
	}
	// 保存的文件必须 600（含 token/密码），加载不告警即已验证权限，这里再显式确认
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("保存权限 = %04o, want 0600", perm)
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("VPSCTL_ACCOUNTS", "")
	home, _ := os.UserHomeDir()
	t.Setenv("HOME", home) // os.UserHomeDir 在 linux 读 $HOME

	got, err := DefaultPath("")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "vpsctl", "accounts.json"); got != want {
		t.Errorf("默认路径 = %q, want %q", got, want)
	}

	t.Setenv("VPSCTL_ACCOUNTS", "/tmp/from-env.json")
	got, _ = DefaultPath("")
	if got != "/tmp/from-env.json" {
		t.Errorf("环境变量未生效: %q", got)
	}

	got, _ = DefaultPath("~/override.json")
	if !strings.HasPrefix(got, home+"/") {
		t.Errorf("~/ 未展开: %q", got)
	}
	if got2, _ := DefaultPath("/abs.json"); got2 != "/abs.json" {
		t.Errorf("override 未生效: %q", got2)
	}
}

func TestFileFindAndRemove(t *testing.T) {
	f := &File{Accounts: []Account{
		{Name: "do-1", Provider: "digitalocean", Token: "t1"},
		{Name: "do-2", Provider: "digitalocean", Token: "t2"},
	}}
	if a := f.Find("do-2"); a == nil || a.Token != "t2" {
		t.Errorf("Find = %+v", a)
	}
	if f.Find("nope") != nil {
		t.Error("不存在应返回 nil")
	}
	// 删除最后一个账号必须拒绝
	if err := f.Remove("do-1"); err != nil {
		t.Errorf("Remove do-1: %v", err)
	}
	if err := f.Remove("do-2"); err == nil || !strings.Contains(err.Error(), "至少保留") {
		t.Errorf("删除最后账号应拒绝: %v", err)
	}
	if len(f.Accounts) != 1 {
		t.Errorf("删除失败时不应变更: %+v", f.Accounts)
	}
}
