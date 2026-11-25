package lighthouse

import (
	"context"
	"crypto/md5" //nolint:gosec // 断言指纹算法的输出，非安全用途
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
)

func runningInstance(id, region string) fakeInstance {
	return fakeInstance{
		id: id, region: region, zone: "ap-guangzhou-3", name: "node-" + id, state: "RUNNING",
		bundle: "bundle_small", platform: "UBUNTU", osName: "Ubuntu Server 24.04 LTS 64bit",
		publicIP: "203.0.113.10", expired: "2026-12-24T12:00:00Z",
	}
}

// countAction 统计某接口被调用的次数。
func countAction(f *fakeAPI, action string) int {
	n := 0
	for _, a := range f.actions() {
		if strings.HasSuffix(a, " "+action) {
			n++
		}
	}
	return n
}

// dig 取请求体里嵌套的字段。
func dig(t *testing.T, body map[string]any, keys ...string) any {
	t.Helper()
	var cur any = body
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("请求体里没有 %v: %v", keys, body)
		}
		cur = m[k]
	}
	return cur
}

// fingerprintOf 按测试期望的格式（小写冒号分隔十六进制）算一段字节的 MD5。
func fingerprintOf(t *testing.T, raw []byte) string {
	t.Helper()
	sum := md5.Sum(raw) //nolint:gosec
	parts := make([]string, 0, len(sum))
	for _, b := range sum {
		parts = append(parts, fmt.Sprintf("%02x", b))
	}
	return strings.Join(parts, ":")
}

/* ---- 列表 / 查询 ---- */

func TestListMapsInstances(t *testing.T) {
	f, c := seeded(t)
	f.bundles["ap-beijing"] = f.bundles["ap-guangzhou"]
	f.add(fakeInstance{id: "lhins-a", region: "ap-guangzhou", name: "a", state: "RUNNING", bundle: "bundle_small",
		platform: "UBUNTU", osName: "Ubuntu Server 24.04 LTS 64bit", publicIP: "203.0.113.1", expired: "2026-12-24T12:00:00Z",
		tags: []tag{{Key: "batch", Value: "x"}, {Key: "bare"}}})
	f.add(fakeInstance{id: "lhins-b", region: "ap-beijing", name: "b", state: "STOPPED", bundle: "bundle_big",
		platform: "DEBIAN", osName: "Debian 12.0 64bit"})

	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("共 %d 台, want 2: %+v", len(got), got)
	}
	byID := map[string]provider.Server{}
	for _, s := range got {
		byID[s.ID] = s
	}
	a := byID["lhins-a"]
	if a.Account != "acct-1" || a.Status != "active" || a.Region != "ap-guangzhou" || a.Size != "bundle_small" ||
		a.VCPUs != 2 || a.MemoryMB != 2048 || a.DiskGB != 40 || a.IPv4Public != "203.0.113.1" || a.IPv4Private != "10.0.0.5" {
		t.Errorf("字段映射不符: %+v", a)
	}
	// 月价来自套餐表（折扣价优先），币种 CNY；到期时间、镜像 slug、坐标都填上
	if a.PriceMonthly != 35 || a.Currency != "CNY" || a.Image != "ubuntu-24.04" ||
		!a.ExpiresAt.Equal(time.Date(2026, 12, 24, 12, 0, 0, 0, time.UTC)) || a.Lat != 23.13 || a.Lng != 113.26 {
		t.Errorf("月费 / 到期 / 镜像 / 坐标不符: %+v", a)
	}
	if !slices.Equal(a.Tags, []string{"batch:x", "bare"}) {
		t.Errorf("标签 = %v", a.Tags)
	}
	if b := byID["lhins-b"]; b.Status != "off" || b.PriceMonthly != 55 || b.Image != "debian-12" {
		t.Errorf("北京实例映射不符: %+v", b)
	}
	// 套餐表按地域缓存：再列一次不会重打 DescribeBundles / DescribeRegions
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countAction(f, "DescribeBundles"); n != 2 {
		t.Errorf("DescribeBundles 应每个有节点的地域各 1 次, got %d", n)
	}
	if n := countAction(f, "DescribeRegions"); n != 1 {
		t.Errorf("DescribeRegions 应只取 1 次, got %d", n)
	}
}

func TestListRegionFailureFailsWholeList(t *testing.T) {
	f, c := seeded(t)
	f.failList["ap-beijing"] = true
	if _, err := c.List(context.Background()); err == nil || !strings.Contains(err.Error(), "ap-beijing") {
		t.Errorf("任一地域失败应整体失败并点名地域, got %v", err)
	}
}

// TestGetLocatesWhenNotIndexed：进程刚起、没 List 过就按 ID 操作，应找到并登记，
// 之后不再逐地域扫。
func TestGetLocatesWhenNotIndexed(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("lhins-a", "ap-guangzhou"))

	s, err := c.Get(context.Background(), "lhins-a")
	if err != nil || s.ID != "lhins-a" || s.Region != "ap-guangzhou" || s.PriceMonthly != 35 {
		t.Fatalf("Get = %+v, %v", s, err)
	}
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	if _, err := c.Get(context.Background(), "lhins-a"); err != nil {
		t.Fatal(err)
	}
	if n := countAction(f, "DescribeInstances"); n != 1 {
		t.Errorf("找到后应登记索引，第二次 Get 只查 1 个地域, got %d 次: %v", n, f.actions())
	}
	if _, err := c.Get(context.Background(), "lhins-none"); !errors.Is(err, errNotFound) {
		t.Errorf("不存在的实例应报 errNotFound, got %v", err)
	}
}

/* ---- 建机 ---- */

func TestCreate(t *testing.T) {
	f, c := seeded(t)
	const password = "Zq7randomPassword9xK"
	s, err := c.Create(context.Background(), provider.CreateRequest{
		Name: "vps-lh-1-ap-guangzhou-01", Region: "ap-guangzhou-4", Size: "bundle_small", Image: "ubuntu-24.04",
		Tags: []string{"batch:20261124T120000Z", "bare"}, Login: provider.Login{Password: password},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.ID != "lhins-test1" || s.Status != "new" || s.Region != "ap-guangzhou" || s.Currency != "CNY" ||
		s.PriceMonthly != 35 || s.IPv4Public != "203.0.113.1" {
		t.Errorf("返回的节点不符: %+v", s)
	}
	dry, real := f.body("CreateInstances_dry"), f.body("CreateInstances_real")
	if dry == nil || real == nil {
		t.Fatalf("应先预检再真建: dry=%v real=%v", dry, real)
	}
	if _, has := dry["ClientToken"]; has || dry["DryRun"] != true {
		t.Errorf("预检应带 DryRun、不带幂等令牌: %v", dry)
	}
	if tok, _ := real["ClientToken"].(string); tok == "" || len(tok) > 64 || real["DryRun"] != nil {
		t.Errorf("真建应带 ≤64 字符的幂等令牌、不带 DryRun: %v", real)
	}
	for label, body := range map[string]map[string]any{"预检": dry, "真建": real} {
		if body["BundleId"] != "bundle_small" || body["BlueprintId"] != "lhbp-u24" || body["InstanceName"] != "vps-lh-1-ap-guangzhou-01" {
			t.Errorf("%s的套餐 / 镜像 / 名称不符: %v", label, body)
		}
		if !slices.Equal(body["Zones"].([]any), []any{"ap-guangzhou-4"}) {
			t.Errorf("%s的可用区不符: %v", label, body["Zones"])
		}
		// 建机即付一个月、到期自动续费
		if dig(t, body, "InstanceChargePrepaid", "Period") != float64(prepaidPeriod) ||
			dig(t, body, "InstanceChargePrepaid", "RenewFlag") != "NOTIFY_AND_AUTO_RENEW" {
			t.Errorf("%s应预付 1 个月 + 自动续费: %v", label, body["InstanceChargePrepaid"])
		}
		// 密码经平台的登录设置下发，且要显式声明"不自动生成"
		if dig(t, body, "LoginConfiguration", "Password") != password || dig(t, body, "LoginConfiguration", "AutoGeneratePassword") != "NO" {
			t.Errorf("%s的登录设置不符: %v", label, body["LoginConfiguration"])
		}
		if tags, _ := body["Tags"].([]any); len(tags) != 2 ||
			tags[0].(map[string]any)["Key"] != "batch" || tags[0].(map[string]any)["Value"] != "20261124T120000Z" ||
			tags[1].(map[string]any)["Key"] != "bare" || tags[1].(map[string]any)["Value"] != "" {
			t.Errorf("%s的标签不符: %v", label, body["Tags"])
		}
		// 打开 root 登录的命令：base64 的固定脚本，不带明文密码
		raw, err := base64.StdEncoding.DecodeString(dig(t, body, "InitCommand", "Content").(string))
		if err != nil || string(raw) != rootLoginScript {
			t.Errorf("%s的自动执行命令不符: %v", label, err)
		}
		if strings.Contains(string(raw), password) || strings.Contains(body["InitCommand"].(map[string]any)["Content"].(string), password) {
			t.Errorf("%s的自动执行命令里不应出现明文密码", label)
		}
		if dig(t, body, "InitCommand", "Timeout") != float64(initCommandTimeout) {
			t.Errorf("%s的自动执行命令超时不符: %v", label, body["InitCommand"])
		}
	}
}

// TestRootLoginScript：会留在平台命令记录里，必须是纯 ASCII 且不含任何密钥操作之外的
// 意外内容——把默认用户的密码哈希复制给 root、打开 root 密码登录。
func TestRootLoginScript(t *testing.T) {
	for i := 0; i < len(rootLoginScript); i++ {
		if rootLoginScript[i] > 0x7F {
			t.Fatalf("脚本含非 ASCII 字节（偏移 %d）", i)
		}
	}
	for _, want := range []string{"usermod -p \"$hash\" root", "PermitRootLogin yes", "PasswordAuthentication yes"} {
		if !strings.Contains(rootLoginScript, want) {
			t.Errorf("脚本缺 %q", want)
		}
	}
	if strings.Contains(rootLoginScript, "chpasswd") || strings.Contains(rootLoginScript, "passwd root") {
		t.Error("脚本不应自行设置明文密码（只能复制密码哈希）")
	}
	// sshd 对每条指令取先读到的值，sshd_config.d 的分片又排在主配置之前：分片必须排在
	// 最前（00-），否则会被镜像自带的 50-cloud-init.conf / 60-cloudimg-settings.conf
	// （PasswordAuthentication no）压住，root 密码登录照样打不开。
	if !strings.Contains(rootLoginScript, "/etc/ssh/sshd_config.d/00-vpsctl-root.conf") {
		t.Error("脚本写入的 sshd 分片应以 00- 开头，保证最先被读到")
	}
	for i := 0; i+1 < len(rootLoginScript); i++ {
		if rootLoginScript[i] == '/' && strings.HasPrefix(rootLoginScript[i:], "/etc/ssh/sshd_config.d/") &&
			!strings.HasPrefix(rootLoginScript[i:], "/etc/ssh/sshd_config.d/00-") {
			t.Errorf("脚本写了不以 00- 开头的 sshd 分片: %q", rootLoginScript[i:i+34])
		}
	}
}

// TestCreateDryRunFailureAborts：预检不过就不真建——建一次就付一个月，不能带病下单。
func TestCreateDryRunFailureAborts(t *testing.T) {
	f, c := seeded(t)
	f.dryRunCode = "ResourceInsufficient.BundleSoldOut"
	_, err := c.Create(context.Background(), provider.CreateRequest{
		Name: "n", Region: "ap-guangzhou", Size: "bundle_small", Image: "ubuntu-24.04", Login: provider.Login{Password: "pw12345678"},
	})
	if err == nil || !strings.Contains(err.Error(), "DryRun") || !strings.Contains(err.Error(), "BundleSoldOut") {
		t.Fatalf("应带出预检的原始错误, got %v", err)
	}
	if len(f.instances) != 0 || f.body("CreateInstances_real") != nil {
		t.Errorf("预检失败不应真建: instances=%d", len(f.instances))
	}
}

func TestPrecheck(t *testing.T) {
	f, c := seeded(t)
	err := c.Precheck(context.Background(), provider.CreateRequest{
		Name: "n", Region: "ap-guangzhou", Size: "bundle_small", Image: "ubuntu-24.04", Login: provider.Login{Password: "pw12345678"},
	})
	if err != nil {
		t.Fatalf("Precheck: %v", err)
	}
	if len(f.instances) != 0 || f.body("CreateInstances_real") != nil {
		t.Error("预检不应建出任何实例")
	}
}

// TestCreateKeyOnly：只给登录密钥时不注入密码，也就不需要打开 root 登录的命令。
func TestCreateKeyOnly(t *testing.T) {
	f, c := seeded(t)
	if _, err := c.Create(context.Background(), provider.CreateRequest{
		Name: "n", Region: "ap-guangzhou", Size: "bundle_small", Image: "lhbp-u22", SSHKeys: []string{"ops"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	real := f.body("CreateInstances_real")
	if !slices.Equal(dig(t, real, "LoginConfiguration", "KeyIds").([]any), []any{"lhkp-1"}) || real["InitCommand"] != nil {
		t.Errorf("密钥登录的建机请求不符: %v", real)
	}
	if real["BlueprintId"] != "lhbp-u22" {
		t.Errorf("lhbp- 开头的镜像 ID 应原样使用: %v", real["BlueprintId"])
	}
}

// TestCreateRejectsUnsupported：没有 cloud-init 的置备在开工前就拒绝，一台都不建。
func TestCreateRejectsUnsupported(t *testing.T) {
	base := provider.CreateRequest{Name: "n", Region: "ap-guangzhou", Size: "bundle_small", Image: "ubuntu-24.04", Login: provider.Login{Password: "pw12345678"}}
	cases := map[string]struct {
		mutate  func(*provider.CreateRequest)
		wantSub string
	}{
		"高位口":           {func(r *provider.CreateRequest) { r.SSHPort = 40222 }, "SSH 端口"},
		"443 隧道":        {func(r *provider.CreateRequest) { r.Tunnel443 = true }, "443"},
		"运维公钥":          {func(r *provider.CreateRequest) { r.AuthorizedKeys = []string{"ssh-ed25519 AAAA"} }, "运维公钥"},
		"自定义 user-data": {func(r *provider.CreateRequest) { r.Login = provider.Login{}; r.UserData = "#cloud-config\n" }, "user-data"},
		"非 root 用户":     {func(r *provider.CreateRequest) { r.Login.User = "deploy" }, "deploy"},
		"密钥与密码同给":       {func(r *provider.CreateRequest) { r.SSHKeys = []string{"ops"} }, "互斥"},
		"云服务器的机型":       {func(r *provider.CreateRequest) { r.Size = "SA5.MEDIUM2" }, "套餐 ID"},
		"不存在的镜像":        {func(r *provider.CreateRequest) { r.Image = "ubuntu-99.04" }, "ubuntu-24.04"},
		"不存在的可用区":       {func(r *provider.CreateRequest) { r.Region = "ap-guangzhou-9" }, "ap-guangzhou-9"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, c := seeded(t)
			req := base
			tc.mutate(&req)
			_, err := c.Create(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("应报错并含 %q, got %v", tc.wantSub, err)
			}
			if len(f.instances) != 0 || f.body("CreateInstances_real") != nil {
				t.Error("被拒的建机一台都不该建")
			}
		})
	}
}

/* ---- 电源 / 动作状态 / 销毁 ---- */

func TestPowerAndActionStatus(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("lhins-a", "ap-guangzhou"))
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	ref, err := c.Power(ctx, "lhins-a", provider.Shutdown)
	if err != nil {
		t.Fatalf("Power: %v", err)
	}
	if ref.ID != "ap-guangzhou|lhins-a|shutdown|" {
		t.Errorf("句柄 = %q", ref.ID)
	}
	if !slices.Equal(f.lastIDs("StopInstances"), []any{"lhins-a"}) {
		t.Errorf("StopInstances 应只带目标实例: %v", f.lastIDs("StopInstances"))
	}
	if st, _ := c.ActionStatus(ctx, ref); st != "in-progress" {
		t.Errorf("关机执行中应为 in-progress, got %q", st)
	}
	if st, err := c.ActionStatus(ctx, ref); st != "completed" || err != nil {
		t.Errorf("关机完成后应为 completed, got %q %v", st, err)
	}
	ref, err = c.Power(ctx, "lhins-a", provider.PowerOn)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := c.ActionStatus(ctx, ref); st != "in-progress" {
		t.Errorf("开机执行中应为 in-progress, got %q", st)
	}
	if st, _ := c.ActionStatus(ctx, ref); st != "completed" {
		t.Errorf("开机完成后应为 completed, got %q", st)
	}
	if _, err := c.Power(ctx, "lhins-a", "reboot"); err == nil {
		t.Error("不支持的动作应报错")
	}
}

// TestPowerRetriesWhileBusy：平台还在收尾上一轮操作时开机会被拒，要等待重试。
func TestPowerRetriesWhileBusy(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("lhins-a", "ap-guangzhou"))
	f.busyStarts = 2
	if _, err := c.Power(context.Background(), "lhins-a", provider.PowerOn); err != nil {
		t.Fatalf("被「操作进行中」拒绝两次后应重试成功: %v", err)
	}
	if n := countAction(f, "StartInstances"); n != 3 {
		t.Errorf("StartInstances 应共发 3 次, got %d", n)
	}
	c.opWait = 0
	f.busyStarts = 5
	if _, err := c.Power(context.Background(), "lhins-a", provider.PowerOn); err == nil || !strings.Contains(err.Error(), "迟迟没有收尾") {
		t.Errorf("等满仍被拒应报错, got %v", err)
	}
}

func TestRebuildRejectedRestoresPower(t *testing.T) {
	f, c := seeded(t)
	inst := f.add(runningInstance("lhins-a", "ap-guangzhou"))
	f.resetReject = true
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.Rebuild(context.Background(), "lhins-a", "ubuntu-22.04")
	if err == nil || !strings.Contains(err.Error(), "reset rejected") || !strings.Contains(err.Error(), "已发起开机") {
		t.Fatalf("应带出平台原因与恢复结果, got %v", err)
	}
	actions := f.actions()
	if i, j := slices.Index(actions, "ap-guangzhou ResetInstance"), slices.Index(actions, "ap-guangzhou StartInstances"); i < 0 || j < i {
		t.Fatalf("重装被拒后应发起开机恢复: %v", actions)
	}
	if inst.osName != "Ubuntu Server 24.04 LTS 64bit" {
		t.Errorf("被拒的重装不应改动系统盘: %q", inst.osName)
	}

	// 本来就已关机：重装被拒后不该替用户开机
	inst.state, inst.pending = "STOPPED", nil
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	if _, err := c.Rebuild(context.Background(), "lhins-a", "ubuntu-22.04"); err == nil {
		t.Fatal("重装被拒应报错")
	}
	if slices.Contains(f.actions(), "ap-guangzhou StartInstances") {
		t.Errorf("已关机的实例重装被拒，不应动电源: %v", f.actions())
	}
}

func TestRebuildPlatformAutoStarts(t *testing.T) {
	f, c := seeded(t)
	inst := f.add(runningInstance("lhins-a", "ap-guangzhou"))
	f.resetAutoStart = true
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	ref, err := c.Rebuild(context.Background(), "lhins-a", "ubuntu-22.04")
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	actions := f.actions()
	i := slices.Index(actions, "ap-guangzhou ResetInstance")
	if i < 0 || slices.Contains(actions[i+1:], "ap-guangzhou StartInstances") {
		t.Errorf("平台自动开机时不应再发开机请求: %v", actions)
	}
	if st, err := c.ActionStatus(context.Background(), ref); st != "completed" || err != nil {
		t.Errorf("自动开机后重装应已完成, got %q %v", st, err)
	}
	_ = inst
}

func TestResize(t *testing.T) {
	f, c := seeded(t)
	inst := f.add(runningInstance("lhins-a", "ap-guangzhou"))
	inst.state = "STOPPED"
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Resize(ctx, "lhins-a", "bundle_big", true); err == nil || !strings.Contains(err.Error(), "扩磁盘") {
		t.Errorf("扩盘请求应明确拒绝, got %v", err)
	}
	if _, err := c.Resize(ctx, "lhins-a", "SA5.MEDIUM4", false); err == nil || !strings.Contains(err.Error(), "套餐 ID") {
		t.Errorf("云服务器的机型名应报错, got %v", err)
	}
	if countAction(f, "ModifyInstancesBundle") != 0 {
		t.Error("被拒的升配不应下发")
	}
	ref, err := c.Resize(ctx, "lhins-a", "bundle_big", false)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if ref.ID != "ap-guangzhou|lhins-a|resize|bundle_big" {
		t.Errorf("句柄 = %q", ref.ID)
	}
	if st, _ := c.ActionStatus(ctx, ref); st != "in-progress" {
		t.Errorf("升配执行中应为 in-progress, got %q", st)
	}
	if st, _ := c.ActionStatus(ctx, ref); st != "completed" {
		t.Errorf("套餐已变应为 completed, got %q", st)
	}
	if inst.bundle != "bundle_big" {
		t.Errorf("套餐未变更: %q", inst.bundle)
	}
}

func TestDelete(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("lhins-a", "ap-guangzhou"))
	f.add(runningInstance("lhins-b", "ap-beijing"))
	ctx := context.Background()
	if err := c.Delete(ctx, "lhins-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if f.instances["lhins-a"] != nil {
		t.Error("实例未被销毁")
	}
	if !slices.Equal(f.lastIDs("TerminateInstances"), []any{"lhins-a"}) {
		t.Errorf("TerminateInstances 应只带目标实例: %v", f.lastIDs("TerminateInstances"))
	}
	// 已不存在视为成功；未登记过的 ID 也能自己找到地域
	if err := c.Delete(ctx, "lhins-a"); err != nil {
		t.Errorf("销毁已不存在的实例应视为成功, got %v", err)
	}
	if err := c.Delete(ctx, "lhins-b"); err != nil || f.instances["lhins-b"] != nil {
		t.Errorf("未登记索引的实例应能定位并销毁: %v", err)
	}
}

/* ---- 目录 ---- */

func TestCatalogs(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("lhins-a", "ap-guangzhou"))
	ctx := context.Background()

	regions, err := c.Regions(ctx)
	if err != nil || len(regions) != 2 || regions[1].Slug != "ap-guangzhou" || regions[1].Name != "ap-guangzhou区" {
		t.Errorf("Regions = %+v, %v", regions, err)
	}
	// 目录地域 = 节点最多的地域；只列在售的 Linux 套餐，按月价排序
	sizes, err := c.Sizes(ctx)
	if err != nil || len(sizes) != 1 {
		t.Fatalf("sizes = %+v, %v", sizes, err)
	}
	if s := sizes[0]; s.Slug != "bundle_small" || s.VCPUs != 2 || s.MemoryMB != 2048 || s.DiskGB != 40 || s.PriceMonthly != 35 || s.Currency != "CNY" {
		t.Errorf("size = %+v", s)
	}
	// 纯系统镜像才有资格进目录；预装应用 / Docker 镜像排除
	images, err := c.Images(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var slugs []string
	for _, im := range images {
		slugs = append(slugs, im.Slug)
	}
	if !slices.Equal(slugs, []string{"debian-12", "debian-13.02", "ubuntu-22.04", "ubuntu-24.04"}) {
		t.Errorf("镜像目录 = %v", slugs)
	}
	if images[3].Distribution != "UBUNTU" || images[3].Name != "Ubuntu Server 24.04 LTS 64bit" {
		t.Errorf("ubuntu-24.04 应指向系统镜像: %+v", images[3])
	}
	keys, err := c.SSHKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].ID != "lhkp-1" || keys[0].Name != "ops" ||
		keys[0].Fingerprint != fingerprintOf(t, []byte{0, 0, 0}) { // "AAAA" 解码是 3 个零字节
		t.Errorf("SSHKeys = %+v, %v", keys, err)
	}
	// 没有节点时目录地域取地域表第一个
	f.mu.Lock()
	delete(f.instances, "lhins-a")
	f.mu.Unlock()
	f.bundles["ap-beijing"] = nil
	c2 := New("acct-1", "AKIDtest", "test-secret", nil)
	c2.api = c.api
	if _, err := c2.Sizes(ctx); err != nil {
		t.Errorf("北京没有套餐不应报错（空目录）: %v", err)
	}
}

func TestStatusAndTags(t *testing.T) {
	cases := map[string]string{
		"RUNNING": "active", "STOPPED": "off", "PENDING": "new", "LAUNCH_FAILED": "error",
		"STOPPING": "stopping", "STARTING": "starting", "TERMINATING": "terminating",
	}
	for in, want := range cases {
		if got := status(in); got != want {
			t.Errorf("status(%s) = %q, want %q", in, got, want)
		}
	}
	kv := tencentTags([]string{"batch:20261124T120000Z", "bare", ":leading", "note:http://a:b"})
	want := []tag{{Key: "batch", Value: "20261124T120000Z"}, {Key: "bare"}, {Key: "note", Value: "http://a:b"}}
	if !slices.Equal(kv, want) {
		t.Errorf("tencentTags = %v, want %v", kv, want)
	}
	if got := tagStrings([]tag{{Key: "batch", Value: "x"}, {Key: "bare"}}); !slices.Equal(got, []string{"batch:x", "bare"}) {
		t.Errorf("tagStrings = %v", got)
	}
	if tencentTags(nil) != nil || tagStrings(nil) != nil {
		t.Error("空标签应保持为空")
	}
	for in, want := range map[string]string{"2026-12-24T12:00Z": "2026-12-24 12:00:00 +0000 UTC", "2026-12-24T12:00:30Z": "2026-12-24 12:00:30 +0000 UTC"} {
		if got := parseTime(in); got.String() != want {
			t.Errorf("parseTime(%q) = %q, want %q", in, got.String(), want)
		}
	}
	if !parseTime("garbage").IsZero() {
		t.Error("解析不了的时间应为零值")
	}
}
