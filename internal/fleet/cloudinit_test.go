package fleet

import (
	"context"
	"strings"
	"testing"
)

func TestCloudInitPasswordShape(t *testing.T) {
	got, err := CloudInitPassword("root", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#cloud-config",
		"ssh_pwauth: true",
		"expire: false",
		"- name: root",
		"password: 's3cret'",
		"type: text",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("生成结果缺 %q:\n%s", want, got)
		}
	}
	if !strings.HasPrefix(got, "#cloud-config\n") {
		t.Errorf("必须以 #cloud-config 头开始:\n%s", got)
	}
}

func TestCloudInitPasswordQuotesYAML(t *testing.T) {
	got, err := CloudInitPassword("root", "it's")
	if err != nil {
		t.Fatal(err)
	}
	// YAML 单引号风格转义：' → ''
	if !strings.Contains(got, "password: 'it''s'") {
		t.Errorf("单引号未按 YAML 规则转义:\n%s", got)
	}
}

func TestCloudInitPasswordDefaults(t *testing.T) {
	got, err := CloudInitPassword("", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "- name: root") {
		t.Errorf("空用户应缺省 root:\n%s", got)
	}
	if _, err := CloudInitPassword("root", ""); err == nil {
		t.Error("空密码应报错")
	}
	if _, err := CloudInitPassword("ro\not", "pw"); err == nil {
		t.Error("用户名含换行应报错")
	}
	if _, err := CloudInitPassword("root", "pw\nx"); err == nil {
		t.Error("密码含换行应报错（无法安全注入 YAML）")
	}
}

// 端口缺省（0/22）不得改变既有输出——CloudInitPassword 是本函数的零端口包装。
func TestCloudInitZeroPortMatchesPassword(t *testing.T) {
	base, _ := CloudInitPassword("root", "pw")
	for _, p := range []int{0, 22} {
		got, err := CloudInit("root", "pw", p)
		if err != nil {
			t.Fatal(err)
		}
		if got != base {
			t.Errorf("ssh_port=%d 应等价于不设端口:\n--- got ---\n%s\n--- base ---\n%s", p, got, base)
		}
		if strings.Contains(got, "sshd_config") || strings.Contains(got, "ssh.socket") {
			t.Errorf("ssh_port=%d 不应触碰 sshd 配置:\n%s", p, got)
		}
	}
}

// 非缺省端口：必须禁 socket activation 再按端口拉起（Ubuntu 24.04 P7 形态）。
func TestCloudInitHighPortInjectsshd(t *testing.T) {
	got, err := CloudInit("root", "pw", 40222)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Port 40222",
		"PasswordAuthentication yes",
		"disable, --now, ssh.socket",
		"99-vpsctl.conf",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("高位口 cloud-init 缺 %q:\n%s", want, got)
		}
	}
	// 必须仍从 #cloud-config 头开始（runcmd 是顶层 YAML 键，不能破头）
	if !strings.HasPrefix(got, "#cloud-config\n") {
		t.Errorf("必须以 #cloud-config 头开始:\n%s", got)
	}
}

func TestCloudInitPortRange(t *testing.T) {
	for _, bad := range []int{-1, 65536} {
		if _, err := CloudInit("root", "pw", bad); err == nil {
			t.Errorf("ssh_port=%d 越界应报错", bad)
		}
	}
}

func TestCreatePerAccountUserDataOverrides(t *testing.T) {
	fa, fb := &fakeProvider{}, &fakeProvider{}
	o := Options{
		Clients: []AccountClient{{Name: "a1", Provider: fa}, {Name: "b1", Provider: fb}},
		Count:   1, Prefix: "vps", Region: "r", Size: "s", Image: "i",
		UserData: "global",
		// 账号级 user-data（如密码注入）优先于全局
		UserDataByAccount: map[string]string{"a1": "acct-a"},
	}
	if _, err := Create(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if len(fa.creates) != 1 || fa.creates[0].UserData != "acct-a" {
		t.Errorf("账号 a1 应使用专属 user-data: %+v", fa.creates)
	}
	if len(fb.creates) != 1 || fb.creates[0].UserData != "global" {
		t.Errorf("账号 b1 应落回全局 user-data: %+v", fb.creates)
	}
}

// 建机注入的端口要回填进结果 JSON（供 NMS 导入载荷逐节点取用）。
func TestCreateFillsSSHPort(t *testing.T) {
	f := &fakeProvider{}
	o := Options{
		Clients: []AccountClient{{Name: "a1", Provider: f}},
		Count:   1, Prefix: "vps", Region: "r", Size: "s", Image: "i",
		SSHPort: 40222,
	}
	res, err := Create(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 || res.Created[0].SSHPort != 40222 {
		t.Errorf("结果 JSON 应回填 ssh_port=40222: %+v", res.Created)
	}
}
