package nodepass

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hoshinojian/vpsctl/internal/fleet"
	"github.com/hoshinojian/vpsctl/internal/provider"
)

func TestGenerate(t *testing.T) {
	shape := regexp.MustCompile(`^[A-Z0-9a-z]+$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		pw, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != passwordLength || !shape.MatchString(pw) {
			t.Fatalf("密码形态不符: %q", pw)
		}
		var upper, lower, digit bool
		for _, r := range pw {
			switch {
			case r >= 'A' && r <= 'Z':
				upper = true
			case r >= 'a' && r <= 'z':
				lower = true
			case r >= '0' && r <= '9':
				digit = true
			}
		}
		if !upper || !lower || !digit {
			t.Errorf("密码应三类字符都有: %q", pw)
		}
		if seen[pw] {
			t.Fatalf("密码重复: %q", pw)
		}
		seen[pw] = true
	}
	// 三类不能固定落在前三位（那会变成可预测的规律）：50 次里类别顺序不该只有一种
	orders := map[string]int{}
	for i := 0; i < 50; i++ {
		pw, _ := Generate()
		orders[classOf(pw[0])+classOf(pw[1])+classOf(pw[2])]++
	}
	if len(orders) == 1 {
		t.Errorf("前三位的字符类别顺序应被打乱, got %v", orders)
	}
}

func classOf(b byte) string {
	switch {
	case b >= 'A' && b <= 'Z':
		return "U"
	case b >= 'a' && b <= 'z':
		return "L"
	}
	return "D"
}

func TestPathFor(t *testing.T) {
	if got := PathFor("/home/u/.config/vpsctl/accounts.json"); got != filepath.Join("/home/u/.config/vpsctl", FileName) {
		t.Errorf("PathFor = %q", got)
	}
}

func TestStoreLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := Open(path)
	s.now = func() time.Time { return time.Date(2026, 11, 24, 12, 0, 0, 500, time.FixedZone("CST", 8*3600)) }

	// 文件不存在：空库，不报错
	nodes, err := s.Load()
	if err != nil || len(nodes) != 0 {
		t.Fatalf("空库 Load = %v, %v", nodes, err)
	}
	if err := s.ForgetAll([]fleet.Target{{Account: "a", ID: "x"}}); err != nil || fileExists(path) {
		t.Errorf("空库 Forget 应静默成功且不建文件: %v", err)
	}

	// 只给真建出来的节点入库；created 里没有的不入
	generated := map[string]Entry{
		"vps-a1-sgp1-01": {User: "root", Password: "pw1"},
		"vps-a1-sgp1-02": {User: "root", Password: "pw2"},
	}
	created := []fleet.ServerJSON{{Account: "a1", ID: "ins-1", Name: "vps-a1-sgp1-01"}}
	if err := s.RecordCreated(created, generated); err != nil {
		t.Fatalf("RecordCreated: %v", err)
	}
	entries, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("应只入库 1 台: %v", entries)
	}
	e := entries[Key("a1", "ins-1")]
	if e.User != "root" || e.Password != "pw1" || e.CreatedAt != time.Date(2026, 11, 24, 4, 0, 0, 0, time.UTC) {
		t.Errorf("记录不符（时间应存 UTC、精确到秒）: %+v", e)
	}

	// 权限必须 0600；不能留下临时文件
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("密码库权限 = %04o, 应仅属主可读写", perm)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("不应留下临时文件")
	}
	raw, _ := os.ReadFile(path)
	if !regexp.MustCompile(`(?s)^{\n  "nodes": \{\n    "a1/ins-1": \{`).Match(raw) {
		t.Errorf("文件形态不符:\n%s", raw)
	}

	// 另一个"进程"（另一个 Store 实例）写入后，本实例读得到、不会覆盖掉
	other := Open(path)
	if err := other.RecordCreated([]fleet.ServerJSON{{Account: "a2", ID: "i-9", Name: "n"}}, map[string]Entry{"n": {User: "root", Password: "pw9"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCreated([]fleet.ServerJSON{{Account: "a1", ID: "ins-2", Name: "vps-a1-sgp1-02"}}, generated); err != nil {
		t.Fatal(err)
	}
	entries, _ = s.Load()
	if len(entries) != 3 {
		t.Fatalf("两个实例交错写入不应互相覆盖: %v", entries)
	}

	// 删除节点记录
	if err := s.ForgetAll([]fleet.Target{{Account: "a1", ID: "ins-1"}}); err != nil {
		t.Fatal(err)
	}
	entries, _ = s.Load()
	if _, ok := entries[Key("a1", "ins-1")]; ok || len(entries) != 2 {
		t.Errorf("Forget 后记录不符: %v", entries)
	}
}

func TestStoreCorruptFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path).Load(); err == nil {
		t.Fatal("密码库损坏应报错，不能悄悄当成空库回落到别的密码")
	}
}

func TestProvision(t *testing.T) {
	opts := fleet.Options{
		Clients: []fleet.AccountClient{
			{Name: "a1", ProviderName: "tencent-cvm", Provider: &stubProvider{}},
			{Name: "a2", ProviderName: "digitalocean", Provider: &stubProvider{}},
		},
		Prefix: "vps", Region: "sgp1", Count: 2, SSHPort: 40222,
		AuthorizedKeys: []string{"ssh-ed25519 AAAA ops"},
	}
	generated, err := Provision(&opts, map[string]Spec{
		"a1": {User: "deploy", ChpasswdCompat: true},
		"a2": {},
	})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"vps-a1-sgp1-01", "vps-a1-sgp1-02", "vps-a2-sgp1-01", "vps-a2-sgp1-02"}
	for _, n := range names {
		e, ok := generated[n]
		if !ok || len(e.Password) != passwordLength {
			t.Fatalf("缺节点 %s 的凭据: %v", n, generated)
		}
		ud, ok := opts.UserDataByNode[n]
		if !ok {
			t.Fatalf("缺节点 %s 的 cloud-init", n)
		}
		// 每台节点的密码各不相同，且就是写进它自己 cloud-init 的那个
		for _, m := range names {
			if m != n && generated[m].Password == e.Password {
				t.Fatalf("节点 %s 与 %s 的密码相同", n, m)
			}
		}
		if !strings.Contains(ud, "password: '"+e.Password+"'") {
			t.Errorf("节点 %s 的 cloud-init 里的密码与凭据不一致:\n%s", n, ud)
		}
	}
	a1 := generated["vps-a1-sgp1-01"]
	if a1.User != "deploy" || generated["vps-a2-sgp1-01"].User != "root" {
		t.Errorf("用户应取账号配置、缺省 root: %+v / %+v", a1, generated["vps-a2-sgp1-01"])
	}
	ud := opts.UserDataByNode["vps-a1-sgp1-01"]
	if !strings.Contains(ud, "list: |\n    deploy:"+a1.Password+"\n") {
		t.Errorf("登记了兼容老 cloud-init 的账号应同时下发 list 写法:\n%s", ud)
	}
	if strings.Contains(opts.UserDataByNode["vps-a2-sgp1-01"], "list:") {
		t.Error("未登记兼容的账号不应出现 list 写法")
	}
	for _, n := range names {
		ud := opts.UserDataByNode[n]
		if !strings.Contains(ud, "Port 40222") || !strings.Contains(ud, "ssh-ed25519 AAAA ops") {
			t.Errorf("高位口与运维公钥应一并注入 (节点 %s):\n%s", n, ud)
		}
	}
}

// stubProvider 只为凑齐 AccountClient；Provision 不发任何请求。
type stubProvider struct{ provider.Provider }

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestForgetAllWithoutFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := Open(path).ForgetAll([]fleet.Target{{Account: "a", ID: "x"}}); err != nil || fileExists(path) {
		t.Errorf("密码库还不存在时应什么都不做: %v", err)
	}
}

// TestRecordOrPrintDumpsPasswordsOnFailure：落库失败时密码只在本进程内存里，必须原样
// 打到应急输出上；落库成功时一个字都不能打到那里。
func TestRecordOrPrintDumpsPasswordsOnFailure(t *testing.T) {
	dir := t.TempDir()
	created := []fleet.ServerJSON{{Account: "a1", ID: "ins-1", Name: "n1"}}
	generated := map[string]Entry{"n1": {User: "root", Password: "s3cret-noise"}}

	var buf bytes.Buffer
	// 目录不存在 → 写不进去
	err := Open(filepath.Join(dir, "missing", FileName)).RecordOrPrint(created, generated, &buf)
	out := buf.String()
	if err == nil || !strings.Contains(out, "a1/ins-1") || !strings.Contains(out, "s3cret-noise") {
		t.Errorf("落库失败应报错并把密码打出来: err=%v out=%q", err, out)
	}

	buf.Reset()
	if err := Open(filepath.Join(dir, FileName)).RecordOrPrint(created, generated, &buf); err != nil || buf.String() != "" {
		t.Errorf("落库成功不应有应急输出: err=%v out=%q", err, buf.String())
	}
	// 没有应急输出口时只报错，不 panic
	if err := Open(filepath.Join(dir, "missing", FileName)).RecordOrPrint(created, generated, nil); err == nil {
		t.Error("落库失败应报错")
	}
}
