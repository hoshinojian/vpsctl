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
