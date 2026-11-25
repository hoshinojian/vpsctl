package cvm

import (
	"context"
	"crypto/ed25519"
	"crypto/md5" //nolint:gosec // 断言指纹算法的输出，非安全用途
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/hoshinojian/vpsctl/internal/provider"
	"github.com/hoshinojian/vpsctl/internal/provider/tencent"
)

// seeded 起一套三个地域的假环境：北京有默认 VPC 与两个子网、三张镜像、一把密钥；
// 上海只有非默认 VPC；广州什么都没有。两个返回值是假 API 与打到它的被测客户端。
func seeded(t *testing.T) (*fakeAPI, *Client) {
	t.Helper()
	f := newFakeAPI(t)
	f.regions = []string{"ap-shanghai", "ap-guangzhou", "ap-beijing"}
	f.vpcs["ap-beijing"] = []vpc{{VpcId: "vpc-other"}, {VpcId: "vpc-default", IsDefault: true}}
	f.subnets["ap-beijing"] = []subnet{
		{SubnetId: "subnet-8", VpcId: "vpc-default", Zone: "ap-beijing-8", IsDefault: true, AvailableIpAddressCount: 100},
		{SubnetId: "subnet-3", VpcId: "vpc-default", Zone: "ap-beijing-3", AvailableIpAddressCount: 200},
		{SubnetId: "subnet-x", VpcId: "vpc-other", Zone: "ap-beijing-3", AvailableIpAddressCount: 300},
	}
	f.vpcs["ap-shanghai"] = []vpc{{VpcId: "vpc-sh"}}
	f.images["ap-beijing"] = []image{
		{ImageId: "img-uefi", ImageName: "Ubuntu Server 24.04 LTS 64bit UEFI", Platform: "Ubuntu", OsType: "linux", Architecture: "x86_64", ImageState: "NORMAL", ImageType: "PUBLIC_IMAGE"},
		{ImageId: "img-plain", ImageName: "Ubuntu Server 24.04 LTS 64bit", Platform: "Ubuntu", OsType: "linux", Architecture: "x86_64", ImageState: "NORMAL", ImageType: "PUBLIC_IMAGE"},
		{ImageId: "img-app", ImageName: "DeepSeek Harness on Ubuntu24.04", Platform: "Ubuntu", OsType: "linux", Architecture: "x86_64", ImageState: "NORMAL", ImageType: "PUBLIC_IMAGE"},
		{ImageId: "img-win", ImageName: "Windows Server 2022", Platform: "Windows Server", OsType: "windows", Architecture: "x86_64", ImageState: "NORMAL", ImageType: "PUBLIC_IMAGE"},
	}
	f.keyPairs["ap-beijing"] = []keyPair{{KeyPairId: "skey-1", KeyName: "ops", PublicKey: "ssh-rsa AAAA ops@example"}}
	return f, f.client(t)
}

func runningInstance(id, region string) fakeInstance {
	return fakeInstance{
		id: id, region: region, zone: "ap-beijing-8", name: "node-" + id, instanceType: "SA5.MEDIUM2",
		state: "RUNNING", imageID: "img-plain", publicIP: "203.0.113.10",
	}
}

// countAction 统计 reset 之后某接口被调用的次数。
func countAction(f *fakeAPI, action string) int {
	n := 0
	for _, a := range f.actions() {
		if strings.HasSuffix(a, " "+action) {
			n++
		}
	}
	return n
}

// colonHex 把字节串排成 OpenSSH 指纹那样的小写冒号分隔十六进制。
func colonHex(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("%02x", x)
	}
	return strings.Join(parts, ":")
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

/* ---- 纯函数 ---- */

func TestStatus(t *testing.T) {
	cases := map[string]string{
		"RUNNING": "active", "STOPPED": "off", "PENDING": "new", "LAUNCH_FAILED": "error",
		"STOPPING": "stopping", "STARTING": "starting", "TERMINATING": "terminating",
	}
	for in, want := range cases {
		if got := status(in); got != want {
			t.Errorf("status(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestTagsRoundTrip(t *testing.T) {
	in := []string{"batch:20261124T120000Z", "managed-by:demo-nms", "bare"}
	kv := tencentTags(in)
	want := []map[string]string{{"Key": "batch", "Value": "20261124T120000Z"}, {"Key": "managed-by", "Value": "demo-nms"}, {"Key": "bare", "Value": ""}}
	if len(kv) != len(want) {
		t.Fatalf("tencentTags = %v, want %v", kv, want)
	}
	for i := range want {
		if kv[i]["Key"] != want[i]["Key"] || kv[i]["Value"] != want[i]["Value"] {
			t.Errorf("tencentTags[%d] = %v, want %v", i, kv[i], want[i])
		}
	}
	// 只在第一个冒号处切：值里带冒号的（如 URL、时间）保持完整
	if got := tencentTags([]string{"note:http://a:b"})[0]["Value"]; got != "http://a:b" {
		t.Errorf("值应保留后续冒号, got %q", got)
	}
	if got := tagStrings([]tag{{Key: "batch", Value: "x"}, {Key: "bare"}}); !slices.Equal(got, []string{"batch:x", "bare"}) {
		t.Errorf("tagStrings = %v", got)
	}
	if tencentTags(nil) != nil || tagStrings(nil) != nil {
		t.Error("空标签应保持为空")
	}
}

func TestImageSlug(t *testing.T) {
	cases := []struct {
		image image
		want  string
	}{
		{image{Platform: "Ubuntu", ImageName: "Ubuntu Server 24.04 LTS 64bit"}, "ubuntu-24.04"},
		{image{Platform: "Ubuntu", ImageName: "Ubuntu Server 26.04 LTS 64bit"}, "ubuntu-26.04"},
		{image{Platform: "Debian", ImageName: "Debian 12.4 64bit"}, "debian-12.04"},
		{image{Platform: "CentOS", ImageName: "CentOS 7.9 64bit"}, "centos-7.09"},
		{image{Platform: "CentOS Stream", ImageName: "CentOS Stream 9 64bit"}, "centos-stream-9"},
		{image{Platform: "AlmaLinux", ImageName: "AlmaLinux 10.0 64bit"}, "almalinux-10"},
		{image{Platform: "TencentOS", ImageName: "TencentOS Server 3.1 (TK4)"}, "tencentos-3.01"},
	}
	for _, c := range cases {
		if got := tencent.ImageSlug(c.image.Platform, c.image.ImageName); got != c.want {
			t.Errorf("imageSlug(%q) = %q, want %q", c.image.ImageName, got, c.want)
		}
	}
}

// TestCatalogImages：同一 slug 的变体里挑最"素"的 x86_64 镜像；预装应用的镜像
// 与 Windows 不进目录。
func TestCatalogImages(t *testing.T) {
	images := []image{
		{ImageId: "img-uefi", ImageName: "Ubuntu Server 24.04 LTS 64bit UEFI", Platform: "Ubuntu", OsType: "linux", Architecture: "x86_64"},
		{ImageId: "img-arm", ImageName: "Ubuntu Server 24.04 LTS 64bit ARM", Platform: "Ubuntu", OsType: "linux", Architecture: "arm64"},
		{ImageId: "img-plain", ImageName: "Ubuntu Server 24.04 LTS 64bit", Platform: "Ubuntu", OsType: "linux", Architecture: "x86_64"},
		{ImageId: "img-app", ImageName: "DeepSeek Harness on Ubuntu24.04", Platform: "Ubuntu", OsType: "linux", Architecture: "x86_64"},
		{ImageId: "img-win", ImageName: "Windows Server 2022", Platform: "Windows Server", OsType: "windows", Architecture: "x86_64"},
		{ImageId: "img-c", ImageName: "CentOS 7.9 64bit", Platform: "CentOS", OsType: "linux", Architecture: "x86_64"},
	}
	if plainOS(images[3]) || !plainOS(images[2]) || !plainOS(images[5]) {
		t.Error("plainOS 判定不符：应用镜像应被排除，系统镜像应保留")
	}
	bySlug := catalogImages(images)
	if got := bySlug["ubuntu-24.04"].ImageId; got != "img-plain" {
		t.Errorf("ubuntu-24.04 应选素镜像 img-plain, got %q", got)
	}
	if got := bySlug["ubuntu-24.04"]; imageScore(images[0]) <= imageScore(got) {
		t.Error("UEFI 变体评分应高于素镜像")
	}
}

func TestLatestGeneration(t *testing.T) {
	types := []instanceTypeConfig{
		{InstanceFamily: "S5", InstanceType: "S5.MEDIUM2", CPU: 2, Memory: 2},
		{InstanceFamily: "S8", InstanceType: "S8.MEDIUM2", CPU: 2, Memory: 2},
		{InstanceFamily: "S9", InstanceType: "S9.MEDIUM2", CPU: 2, Memory: 2},
		{InstanceFamily: "S9", InstanceType: "S9e.MEDIUM2", CPU: 2, Memory: 2},
		{InstanceFamily: "S9", InstanceType: "S9.LARGE8", CPU: 2, Memory: 8},
		{InstanceFamily: "MA9", InstanceType: "MA9.MEDIUM16", CPU: 2, Memory: 16},
		{InstanceFamily: "BMG5t", InstanceType: "BMG5t.24XLARGE384", CPU: 96, Memory: 384}, // 裸金属，不在收录系列
	}
	var got []string
	for _, t := range latestGeneration(types) {
		got = append(got, t.InstanceType)
	}
	want := []string{"S9.MEDIUM2", "S9e.MEDIUM2", "S9.LARGE8", "MA9.MEDIUM16"}
	if !slices.Equal(got, want) {
		t.Errorf("latestGeneration = %v, want %v", got, want)
	}
	if s, g := parseFamily("SA5"); s != "SA" || g != 5 {
		t.Errorf("parseFamily(SA5) = %q,%d", s, g)
	}
	if s, g := parseFamily("weird"); s != "" || g != 0 {
		t.Errorf("parseFamily(weird) = %q,%d", s, g)
	}
}

func TestHostNameAndToken(t *testing.T) {
	if hostName("vps-tc-1-ap-beijing-01") != "vps-tc-1-ap-beijing-01" {
		t.Error("常规节点名应直接可作主机名")
	}
	for _, bad := range []string{"x", "-lead", "trail-", "a--b", "a..b", "有中文", strings.Repeat("a", 61)} {
		if hostName(bad) != "" {
			t.Errorf("主机名 %q 不合法，应返回空", bad)
		}
	}
	tok := tencent.ClientToken("vps-节点-with-中文-and-a-long-suffix")
	if len(tok) > 64 || !strings.HasPrefix(tok, "vpsctl-") {
		t.Errorf("clientToken = %q（应以 vpsctl- 开头且 ≤64）", tok)
	}
	for _, r := range tok {
		if r > 126 || r < 33 {
			t.Errorf("clientToken 含非 ASCII 可打印字符: %q", tok)
		}
	}
	if tencent.ClientToken("a") == tencent.ClientToken("a") {
		t.Error("两次生成的幂等令牌不应相同")
	}
}

func TestSSHFingerprint(t *testing.T) {
	// 现场造一把 ed25519 公钥，按 SSH 线格式拼出公钥体：指纹 = 体的 MD5，小写冒号分隔
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blob := append([]byte{0, 0, 0, 11}, []byte("ssh-ed25519")...)
	blob = append(append(blob, 0, 0, 0, 32), pub...)
	sum := md5.Sum(blob) //nolint:gosec
	want := colonHex(sum[:])
	line := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " me@host"
	if got := tencent.SSHFingerprint(line); got != want {
		t.Errorf("sshFingerprint = %q, want %q", got, want)
	}
	// 只取第二段当公钥体；段数不够或不是 base64 时返回空串
	if got := tencent.SSHFingerprint("ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " trailing:colon"); got != want {
		t.Errorf("带冒号的注释不应影响指纹, got %q", got)
	}
	for _, bad := range []string{"", "ssh-rsa", "ssh-rsa !!! not-base64"} {
		if got := tencent.SSHFingerprint(bad); got != "" {
			t.Errorf("tencent.SSHFingerprint(%q) = %q, want 空", bad, got)
		}
	}
}

/* ---- 列表 / 查询 ---- */

func TestListAcrossRegions(t *testing.T) {
	f, c := seeded(t)
	f.add(fakeInstance{id: "ins-a", region: "ap-beijing", zone: "ap-beijing-8", name: "a", instanceType: "SA5.MEDIUM2",
		state: "RUNNING", imageID: "img-plain", publicIP: "203.0.113.1", tags: []tag{{Key: "batch", Value: "x"}, {Key: "bare"}}})
	f.add(fakeInstance{id: "ins-b", region: "ap-beijing", zone: "ap-beijing-8", name: "b", instanceType: "SA5.MEDIUM2",
		state: "STOPPED", imageID: "img-plain"})
	f.add(fakeInstance{id: "ins-c", region: "ap-shanghai", name: "c", instanceType: "S5.MEDIUM2", state: "PENDING", imageID: "img-plain"})
	// 包年包月实例：与 ins-a 同规格、同可用区，但不能借用它的按量询价
	f.add(fakeInstance{id: "ins-d", region: "ap-beijing", zone: "ap-beijing-8", name: "d", instanceType: "SA5.MEDIUM2",
		state: "RUNNING", imageID: "img-plain", charge: "PREPAID"})

	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("共 %d 台, want 4: %+v", len(got), got)
	}
	byID := map[string]provider.Server{}
	for _, s := range got {
		byID[s.ID] = s
	}
	a := byID["ins-a"]
	if a.Account != "acct-1" || a.Status != "active" || a.Region != "ap-beijing" || a.Size != "SA5.MEDIUM2" ||
		a.VCPUs != 2 || a.MemoryMB != 2048 || a.DiskGB != 50 || a.IPv4Public != "203.0.113.1" || a.IPv4Private != "10.0.0.5" {
		t.Errorf("字段映射不符: %+v", a)
	}
	// 询价 0.2 元/小时 → 月费 146，币种 CNY；镜像标注成 slug；坐标取自地域表
	if a.PriceMonthly != 146 || a.Currency != "CNY" || a.Image != "ubuntu-24.04" || a.Lat != 39.90 || a.Lng != 116.41 {
		t.Errorf("月费/币种/镜像/坐标不符: %+v", a)
	}
	// 按量节点另外带出真实计费单位：每小时单价
	if a.Billing != provider.BillingHourly || a.PriceHourly != 0.2 {
		t.Errorf("计费方式 / 小时价不符: billing=%q hourly=%v", a.Billing, a.PriceHourly)
	}
	// 包年包月的 CVM 本工具不询价（续费价没在真机上验过）：既不标计费方式也不带价
	if d := byID["ins-d"]; d.Billing != "" || d.PriceMonthly != 0 || d.PriceHourly != 0 {
		t.Errorf("包年包月实例不应带按量的价: %+v", d)
	}
	if !slices.Equal(a.Tags, []string{"batch:x", "bare"}) {
		t.Errorf("标签 = %v", a.Tags)
	}
	if byID["ins-b"].Status != "off" || byID["ins-c"].Status != "new" || byID["ins-c"].Region != "ap-shanghai" {
		t.Errorf("状态归一不符: %+v / %+v", byID["ins-b"], byID["ins-c"])
	}
	if a.CreatedAt.IsZero() {
		t.Error("创建时间未解析")
	}

	// 地域表只取一次；List 登记的索引让后续按 ID 查询不再逐地域扫
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countAction(f, "DescribeRegions"); n != 1 {
		t.Errorf("DescribeRegions 应只调 1 次, got %d", n)
	}
	f.reset()
	if _, err := c.Get(context.Background(), "ins-c"); err != nil {
		t.Fatal(err)
	}
	for _, a := range f.actions() {
		if strings.HasSuffix(a, " DescribeInstances") && !strings.HasPrefix(a, "ap-shanghai ") {
			t.Errorf("索引已登记，Get 不应再查别的地域: %s", a)
		}
	}
}

func TestListRegionFailureFailsWholeList(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("ins-a", "ap-beijing"))
	f.failList["ap-guangzhou"] = true
	if _, err := c.List(context.Background()); err == nil || !strings.Contains(err.Error(), "ap-guangzhou") {
		t.Errorf("任一地域失败应整体失败并点名地域, got %v", err)
	}
}

// TestGetLocatesWhenNotIndexed：进程刚起、没 List 过就按 ID 操作，应逐地域并发
// 找到并登记，之后不再扫。
func TestGetLocatesWhenNotIndexed(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("ins-a", "ap-beijing"))

	s, err := c.Get(context.Background(), "ins-a")
	if err != nil || s.ID != "ins-a" || s.Region != "ap-beijing" {
		t.Fatalf("Get = %+v, %v", s, err)
	}
	f.reset()
	if _, err := c.Get(context.Background(), "ins-a"); err != nil {
		t.Fatal(err)
	}
	if n := countAction(f, "DescribeInstances"); n != 1 {
		t.Errorf("找到后应登记索引，第二次 Get 只查 1 个地域, got %d 次: %v", n, f.actions())
	}
}

func TestGetNotFound(t *testing.T) {
	f, c := seeded(t)
	if _, err := c.Get(context.Background(), "ins-none"); !errors.Is(err, errNotFound) {
		t.Errorf("不存在的实例应报 errNotFound, got %v", err)
	}
	if n := len(f.regions); countAction(f, "DescribeInstances") != n {
		t.Errorf("应查遍全部 %d 个地域, got %d", n, countAction(f, "DescribeInstances"))
	}
}

/* ---- 电源与动作状态 ---- */

func TestPowerAndActionStatus(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("ins-a", "ap-beijing"))
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 优雅关机：软关机，不停计费（保住公网 IP）
	ref, err := c.Power(ctx, "ins-a", provider.Shutdown)
	if err != nil {
		t.Fatalf("Power: %v", err)
	}
	if ref.ID != "ap-beijing|ins-a|shutdown|" {
		t.Errorf("句柄 = %q", ref.ID)
	}
	body := f.lastBody("StopInstances")
	if dig(t, body, "StopType") != "SOFT" || dig(t, body, "StoppedMode") != "KEEP_CHARGING" {
		t.Errorf("优雅关机应为 SOFT + KEEP_CHARGING: %v", body)
	}
	if st, _ := c.ActionStatus(ctx, ref); st != "in-progress" {
		t.Errorf("关机执行中应为 in-progress, got %q", st)
	}
	if st, err := c.ActionStatus(ctx, ref); st != "completed" || err != nil {
		t.Errorf("关机完成后应为 completed, got %q %v", st, err)
	}

	// 硬断电
	if _, err := c.Power(ctx, "ins-a", provider.PowerOff); err != nil {
		t.Fatal(err)
	}
	if got := dig(t, f.lastBody("StopInstances"), "StopType"); got != "HARD" {
		t.Errorf("power_off 应为 HARD, got %v", got)
	}

	// 开机
	ref, err = c.Power(ctx, "ins-a", provider.PowerOn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ref.ID, "|power_on|") {
		t.Errorf("句柄 = %q", ref.ID)
	}
	if st, _ := c.ActionStatus(ctx, ref); st != "in-progress" {
		t.Errorf("开机执行中应为 in-progress, got %q", st)
	}
	if st, _ := c.ActionStatus(ctx, ref); st != "completed" {
		t.Errorf("开机完成后应为 completed, got %q", st)
	}

	if _, err := c.Power(ctx, "ins-a", "reboot"); err == nil {
		t.Error("不支持的动作应报错")
	}
}

// TestActionStatusFailure：只认本动作的操作失败；上一轮留下的旧失败不定罪。
func TestActionStatusFailure(t *testing.T) {
	f, c := seeded(t)
	inst := f.add(runningInstance("ins-a", "ap-beijing"))
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref := provider.EncodeActionRef("ap-beijing", "ins-a", kindPowerOn, "")

	inst.state, inst.pending = "STOPPED", nil
	inst.latestOp, inst.opState, inst.opErr = "ResetInstancesType", "OPERATE_FAILED", "old failure"
	if st, err := c.ActionStatus(ctx, ref); st != "in-progress" || err != nil {
		t.Errorf("上一轮的失败不应算到开机头上: %q %v", st, err)
	}
	inst.latestOp = "StartInstances"
	st, err := c.ActionStatus(ctx, ref)
	if st != "errored" || err == nil || !strings.Contains(err.Error(), "old failure") {
		t.Errorf("本动作失败应报 errored 并带原因: %q %v", st, err)
	}

	inst.latestOp, inst.opState, inst.opErr = "StartInstances", "OPERATE_SUCCESS", ""
	inst.state = "RUNNING"
	if st, err := c.ActionStatus(ctx, provider.EncodeActionRef("ap-beijing", "ins-a", "nope", "")); st != "" || err == nil {
		t.Error("未知动作种类应报错")
	}
}

/* ---- 销毁 ---- */

func TestDelete(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("ins-a", "ap-beijing"))
	f.add(runningInstance("ins-b", "ap-shanghai"))
	ctx := context.Background()
	if err := c.Delete(ctx, "ins-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if f.get("ins-a") != nil {
		t.Error("实例未被销毁")
	}
	if got, _ := dig(t, f.lastBody("TerminateInstances"), "InstanceIds").([]any); !slices.Equal(got, []any{"ins-a"}) {
		t.Errorf("TerminateInstances 应只带目标实例: %v", got)
	}
	// 已不存在视为成功（批量重试幂等）；未登记过的 ID 也能自己找到地域
	if err := c.Delete(ctx, "ins-a"); err != nil {
		t.Errorf("销毁已不存在的实例应视为成功, got %v", err)
	}
	if err := c.Delete(ctx, "ins-b"); err != nil || f.get("ins-b") != nil {
		t.Errorf("未登记索引的实例应能定位并销毁: %v", err)
	}
}

/* ---- 建机 ---- */

func TestCreate(t *testing.T) {
	f, c := seeded(t)
	f.diskOK = map[string]bool{"CLOUD_PREMIUM": true} // BSSD 被拒 → 顺延到高性能云盘
	userData := "#cloud-config\nssh_pwauth: true\n"

	s, err := c.Create(context.Background(), provider.CreateRequest{
		Name: "vps-tc-1-ap-beijing-01", Region: "ap-beijing", Size: "SA5.MEDIUM2", Image: "ubuntu-24.04",
		Tags: []string{"batch:20261124T120000Z", "env"}, UserData: userData, SSHKeys: []string{"ops", "skey-9"}, Monitoring: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.ID != "ins-test0001" || s.Status != "new" || s.Region != "ap-beijing" || s.Currency != "CNY" ||
		s.Image != "ubuntu-24.04" || s.IPv4Public != "203.0.113.1" {
		t.Errorf("返回的节点不符: %+v", s)
	}

	var dry, real map[string]any
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].action != "RunInstances" {
			continue
		}
		if _, isDry := f.calls[i].body["DryRun"]; isDry {
			if dry == nil {
				dry = f.calls[i].body
			}
		} else {
			real = f.calls[i].body
		}
	}
	if dry == nil || real == nil {
		t.Fatalf("应先预检再真建: %v", f.actions())
	}
	if _, has := dry["ClientToken"]; has {
		t.Error("预检不应带幂等令牌（与正式建机共用会撞）")
	}
	if tok, _ := real["ClientToken"].(string); tok == "" || len(tok) > 64 {
		t.Errorf("正式建机应带 ≤64 字符的幂等令牌, got %q", real["ClientToken"])
	}
	if real["InstanceChargeType"] != postpaid {
		t.Errorf("应按量计费, got %v", real["InstanceChargeType"])
	}
	if real["ImageId"] != "img-plain" {
		t.Errorf("ubuntu-24.04 应解析成素镜像 img-plain, got %v", real["ImageId"])
	}
	// 未指定可用区 → 落在默认子网所在的 ap-beijing-8；网络取默认 VPC
	if got := dig(t, real, "Placement", "Zone"); got != "ap-beijing-8" {
		t.Errorf("可用区 = %v, want ap-beijing-8", got)
	}
	if got := dig(t, real, "VirtualPrivateCloud", "VpcId"); got != "vpc-default" {
		t.Errorf("VpcId = %v", got)
	}
	if got := dig(t, real, "VirtualPrivateCloud", "SubnetId"); got != "subnet-8" {
		t.Errorf("SubnetId = %v", got)
	}
	// 云盘类型：预检被拒的 BSSD 不能用在正式请求里
	if got := dig(t, real, "SystemDisk", "DiskType"); got != "CLOUD_PREMIUM" {
		t.Errorf("DiskType = %v, want CLOUD_PREMIUM", got)
	}
	if got := dig(t, real, "SystemDisk", "DiskSize"); got != float64(defaultSystemDiskGB) {
		t.Errorf("DiskSize = %v", got)
	}
	if keys, _ := dig(t, real, "LoginSettings", "KeyIds").([]any); !slices.Equal(keys, []any{"skey-1", "skey-9"}) {
		t.Errorf("KeyIds = %v（名称 ops 应解析成 skey-1，ID 原样）", keys)
	}
	ud, _ := real["UserData"].(string)
	if decoded, err := base64.StdEncoding.DecodeString(ud); err != nil || string(decoded) != userData {
		t.Errorf("UserData 应 base64 编码原文, got %q (%v)", ud, err)
	}
	spec := dig(t, real, "TagSpecification").([]any)[0].(map[string]any)
	if spec["ResourceType"] != "instance" {
		t.Errorf("TagSpecification.ResourceType = %v", spec["ResourceType"])
	}
	tags := spec["Tags"].([]any)
	if len(tags) != 2 {
		t.Fatalf("TagSpecification.Tags = %v", tags)
	}
	if kv := tags[0].(map[string]any); kv["Key"] != "batch" || kv["Value"] != "20261124T120000Z" {
		t.Errorf("第 1 个标签 = %v", kv)
	}
	if kv := tags[1].(map[string]any); kv["Key"] != "env" || kv["Value"] != "" {
		t.Errorf("没冒号的标签应整串作键、值留空, got %v", kv)
	}
	if real["HostName"] != "vps-tc-1-ap-beijing-01" {
		t.Errorf("HostName = %v", real["HostName"])
	}
	if got := dig(t, real, "EnhancedService", "MonitorService", "Enabled"); got != true {
		t.Errorf("云监控应随 Monitoring 开启, got %v", got)
	}
	if _, has := real["SecurityGroupIds"]; has {
		t.Error("不应硬指定安全组（交给平台绑定默认安全组）")
	}
}

// TestCreateExplicitZone：区域 slug 给成可用区时按该可用区选子网。
func TestCreateExplicitZone(t *testing.T) {
	f, c := seeded(t)
	if _, err := c.Create(context.Background(), provider.CreateRequest{
		Name: "node-01", Region: "ap-beijing-3", Size: "SA5.MEDIUM2", Image: "img-win",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	body := f.lastBody("RunInstances")
	if got := dig(t, body, "Placement", "Zone"); got != "ap-beijing-3" {
		t.Errorf("Zone = %v", got)
	}
	if got := dig(t, body, "VirtualPrivateCloud", "SubnetId"); got != "subnet-3" {
		t.Errorf("应选 ap-beijing-3 的子网, got %v", got)
	}
	if body["ImageId"] != "img-win" {
		t.Error("img- 开头的应原样当作 ImageId")
	}
}

func TestCreateErrors(t *testing.T) {
	ctx := context.Background()
	base := provider.CreateRequest{Name: "node-01", Size: "SA5.MEDIUM2", Image: "ubuntu-24.04"}

	t.Run("可用区没有子网", func(t *testing.T) {
		f, c := seeded(t)
		f.subnets["ap-beijing"] = f.subnets["ap-beijing"][:1] // 只剩 ap-beijing-8 的子网
		_, err := c.Create(ctx, func() provider.CreateRequest { r := base; r.Region = "ap-beijing-3"; return r }())
		if err == nil || !strings.Contains(err.Error(), "ap-beijing-8") {
			t.Errorf("应报错并列出有子网的可用区, got %v", err)
		}
		if countAction(f, "RunInstances") != 0 {
			t.Error("网络没准备好就不该发建机请求")
		}
	})
	t.Run("地域没有默认VPC", func(t *testing.T) {
		f, c := seeded(t)
		// 镜像直接给 ImageId，免得先撞上"上海没有这张镜像"
		_, err := c.Create(ctx, provider.CreateRequest{Name: "node-01", Region: "ap-shanghai", Size: "SA5.MEDIUM2", Image: "img-raw"})
		if err == nil || !strings.Contains(err.Error(), "默认 VPC") {
			t.Errorf("应提示先建默认 VPC, got %v", err)
		}
		if countAction(f, "DescribeVpcs") == 0 || countAction(f, "RunInstances") != 0 {
			t.Error("不应代建网络，也不该发建机请求")
		}
	})
	t.Run("预检全不通过", func(t *testing.T) {
		f, c := seeded(t)
		f.diskOK = map[string]bool{}
		_, err := c.Create(ctx, func() provider.CreateRequest { r := base; r.Region = "ap-beijing"; return r }())
		if err == nil || !strings.Contains(err.Error(), "DryRun") || !strings.Contains(err.Error(), "19045") {
			t.Errorf("应带出最后一次预检的原始错误, got %v", err)
		}
		if len(f.instances) != 0 || countAction(f, "RunInstances") != len(diskCandidates) {
			t.Errorf("预检失败不应真建机；预检次数 = %d", countAction(f, "RunInstances"))
		}
	})
	t.Run("区域不存在", func(t *testing.T) {
		_, c := seeded(t)
		_, err := c.Create(ctx, provider.CreateRequest{Name: "node-01", Region: "ap-mars", Size: "SA5.MEDIUM2", Image: "ubuntu-24.04"})
		if err == nil || !strings.Contains(err.Error(), "ap-mars") {
			t.Errorf("应报未知区域, got %v", err)
		}
	})
	t.Run("镜像不存在", func(t *testing.T) {
		_, c := seeded(t)
		_, err := c.Create(ctx, func() provider.CreateRequest { r := base; r.Region = "ap-beijing"; r.Image = "ubuntu-99.04"; return r }())
		if err == nil || !strings.Contains(err.Error(), "ubuntu-24.04") {
			t.Errorf("应报错并列出可用镜像, got %v", err)
		}
	})
}

/* ---- 重装 / 改配 ---- */

func TestRebuildRunningInstance(t *testing.T) {
	f, c := seeded(t)
	inst := f.add(runningInstance("ins-a", "ap-beijing"))
	f.resetPolls = 3 // 重装要三次查询才结束：验证会等它真的结束再开机
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref, err := c.Rebuild(ctx, "ins-a", "ubuntu-24.04")
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	actions := f.actions()
	i := slices.Index(actions, "ap-beijing StopInstances")
	if i < 0 || !slices.Contains(actions, "ap-beijing ResetInstance") ||
		slices.Index(actions, "ap-beijing StartInstances") < i {
		t.Fatalf("应按 关机 → 重装 → 开机 的顺序: %v", actions)
	}
	// 重装结束前至少查了 resetPolls 次，不能抢跑开机
	queriesBeforeStart := 0
	for _, a := range actions {
		if a == "ap-beijing StartInstances" {
			break
		}
		if a == "ap-beijing DescribeInstances" {
			queriesBeforeStart++
		}
	}
	if queriesBeforeStart < 2+3 {
		t.Errorf("开机前应至少查询 关机 2 次 + 重装 3 次, got %d: %v", queriesBeforeStart, actions)
	}
	body := f.lastBody("ResetInstance")
	if body["InstanceId"] != "ins-a" || body["ImageId"] != "img-plain" {
		t.Errorf("重装请求不符: %v", body)
	}
	if !strings.Contains(ref.ID, "|rebuild|") {
		t.Errorf("应返回开机的句柄: %q", ref.ID)
	}
	if st, _ := c.ActionStatus(ctx, ref); st != "in-progress" {
		t.Errorf("开机中应为 in-progress, got %q", st)
	}
	if st, _ := c.ActionStatus(ctx, ref); st != "completed" {
		t.Errorf("开机完成应为 completed, got %q", st)
	}
	if inst.imageID != "img-plain" || inst.state != "RUNNING" {
		t.Errorf("重装后镜像应更新、实例应回到运行中: %+v", inst)
	}
}

// TestRebuildStoppedInstanceSkipsPowerOff：已关机的实例直接重装，不画蛇添足。
func TestRebuildStoppedInstanceSkipsPowerOff(t *testing.T) {
	f, c := seeded(t)
	inst := f.add(runningInstance("ins-a", "ap-beijing"))
	inst.state = "STOPPED"
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Rebuild(context.Background(), "ins-a", "ubuntu-24.04"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if actions := f.actions(); slices.Contains(actions, "ap-beijing StopInstances") || !slices.Contains(actions, "ap-beijing StartInstances") {
		t.Errorf("应只 重装 → 开机: %v", actions)
	}
}

// TestRebuildResetFails：重装失败时不能开机，要把平台给的原因带出来。
func TestRebuildResetFails(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("ins-a", "ap-beijing"))
	f.resetFail = true
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.Rebuild(context.Background(), "ins-a", "ubuntu-24.04")
	if err == nil || !strings.Contains(err.Error(), "image not available") {
		t.Fatalf("应带出平台给的失败原因, got %v", err)
	}
	// 重装失败后要把为了重装才关掉的机开回去，且说明是恢复而不是装完开机
	if err == nil || !strings.Contains(err.Error(), "已发起开机") {
		t.Errorf("重装失败应报错并说明已恢复开机, got %v", err)
	}
	if !slices.Contains(f.actions(), "ap-beijing StartInstances") {
		t.Error("重装失败后应发起开机恢复")
	}
}

func TestResize(t *testing.T) {
	f, c := seeded(t)
	inst := f.add(runningInstance("ins-a", "ap-beijing"))
	inst.state = "STOPPED"
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Resize(ctx, "ins-a", "SA5.MEDIUM4", true); err == nil || !strings.Contains(err.Error(), "扩磁盘") {
		t.Errorf("扩盘请求应明确拒绝, got %v", err)
	}
	if countAction(f, "ResetInstancesType") != 0 {
		t.Error("拒绝的改配不应下发")
	}
	ref, err := c.Resize(ctx, "ins-a", "SA5.MEDIUM4", false)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if ref.ID != "ap-beijing|ins-a|resize|SA5.MEDIUM4" {
		t.Errorf("句柄 = %q", ref.ID)
	}
	// 机型还没变、操作在跑 → 进行中；机型变了才算完成
	if st, _ := c.ActionStatus(ctx, ref); st != "in-progress" {
		t.Errorf("改配执行中应为 in-progress, got %q", st)
	}
	if st, _ := c.ActionStatus(ctx, ref); st != "completed" {
		t.Errorf("机型已变应为 completed, got %q", st)
	}
	if inst.instanceType != "SA5.MEDIUM4" {
		t.Errorf("机型未变更: %q", inst.instanceType)
	}
}

/* ---- 目录 ---- */

func TestCatalogs(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("ins-a", "ap-beijing"))
	f.types["ap-beijing"] = []instanceTypeConfig{
		{InstanceFamily: "S5", InstanceType: "S5.MEDIUM2", CPU: 2, Memory: 2},
		{InstanceFamily: "S9", InstanceType: "S9.MEDIUM2", CPU: 2, Memory: 2},
		{InstanceFamily: "S9", InstanceType: "S9.LARGE8", CPU: 2, Memory: 8},
		{InstanceFamily: "S9", InstanceType: "S9.LARGE8", CPU: 2, Memory: 8}, // 接口按可用区重复
		{InstanceFamily: "S9", InstanceType: "S9.2XLARGE32", CPU: 8, Memory: 32},
		{InstanceFamily: "S9", InstanceType: "S9.4XLARGE64", CPU: 16, Memory: 64},
		{InstanceFamily: "GN10X", InstanceType: "GN10X.2XLARGE40", CPU: 8, Memory: 40, GPU: 1},
	}
	ctx := context.Background()

	regions, err := c.Regions(ctx)
	if err != nil || len(regions) != 3 || regions[0].Slug != "ap-shanghai" || regions[0].Name != "ap-shanghai区" {
		t.Errorf("Regions = %+v, %v", regions, err)
	}

	// 目录地域 = 节点最多的地域（ap-beijing），镜像 / 机型 / 密钥都从那里取
	images, err := c.Images(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 三张 Linux 镜像归并成一个 slug（素镜像胜出），Windows 不进目录
	var slugs []string
	for _, im := range images {
		slugs = append(slugs, im.Slug)
	}
	// Windows 与应用镜像不进目录
	if !slices.Equal(slugs, []string{"ubuntu-24.04"}) {
		t.Errorf("镜像目录不符: %v", slugs)
	}
	for _, im := range images {
		if im.Slug == "ubuntu-24.04" && (im.Distribution != "Ubuntu" || im.Name != "Ubuntu Server 24.04 LTS 64bit") {
			t.Errorf("ubuntu-24.04 应指向素镜像: %+v", im)
		}
	}

	sizes, err := c.Sizes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range sizes {
		got = append(got, s.Slug)
	}
	// 去重；只留最新一代；超 8 核与 GPU 机型不收；按 vCPU → 内存 → 名称排序
	if want := []string{"S9.MEDIUM2", "S9.LARGE8", "S9.2XLARGE32"}; !slices.Equal(got, want) {
		t.Errorf("sizes = %v, want %v", got, want)
	}
	if sizes[0].MemoryMB != 2048 || sizes[0].Currency != "CNY" || sizes[0].PriceMonthly != 0 {
		t.Errorf("机型目录不带月费、币种 CNY: %+v", sizes[0])
	}

	keys, err := c.SSHKeys(ctx)
	// "AAAA" 解码是 3 个零字节，指纹即其 MD5
	fp := md5.Sum([]byte{0, 0, 0}) //nolint:gosec
	wantFP := colonHex(fp[:])
	if err != nil || len(keys) != 1 || keys[0].ID != "skey-1" || keys[0].Name != "ops" || keys[0].Fingerprint != wantFP {
		t.Errorf("SSHKeys = %+v, %v", keys, err)
	}
}

// TestRebuildRejectedRestoresPower：平台拒绝重装（如镜像不适配机型）时系统盘没动，
// 为了重装才关掉的机要开回去，错误里带出平台原因；本来就已关机的机则不动电源。
func TestRebuildRejectedRestoresPower(t *testing.T) {
	f, c := seeded(t)
	inst := f.add(runningInstance("ins-a", "ap-beijing"))
	f.resetReject = true
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.Rebuild(context.Background(), "ins-a", "ubuntu-24.04")
	if err == nil || !strings.Contains(err.Error(), "InvalidImageForGivenInstanceType") || !strings.Contains(err.Error(), "已发起开机") {
		t.Fatalf("应带出平台原因与恢复结果, got %v", err)
	}
	actions := f.actions()
	if i, j := slices.Index(actions, "ap-beijing ResetInstance"), slices.Index(actions, "ap-beijing StartInstances"); i < 0 || j < i {
		t.Fatalf("重装被拒后应发起开机恢复: %v", actions)
	}
	if inst.imageID != "img-plain" {
		t.Errorf("被拒的重装不应改动系统盘: %q", inst.imageID)
	}

	// 本来就已关机：重装被拒后不该替用户开机
	inst.state, inst.pending = "STOPPED", nil
	f.reset()
	if _, err := c.Rebuild(context.Background(), "ins-a", "ubuntu-24.04"); err == nil {
		t.Fatal("重装被拒应报错")
	}
	if actions := f.actions(); slices.Contains(actions, "ap-beijing StartInstances") {
		t.Errorf("已关机的实例重装被拒，不应动电源: %v", actions)
	}
}

// TestPowerRetriesWhileBusy：改配刚完成时平台还在收尾，开机会被"操作进行中"拒绝；
// 要等一会儿重试到成功，而不是把失败直接报给编排层。
func TestPowerRetriesWhileBusy(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("ins-a", "ap-beijing"))
	f.busyStarts = 2
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Power(context.Background(), "ins-a", provider.PowerOn); err != nil {
		t.Fatalf("被「操作进行中」拒绝两次后应重试成功: %v", err)
	}
	if n := countAction(f, "StartInstances"); n != 3 {
		t.Errorf("StartInstances 应共发 3 次（2 次被拒 + 1 次成功）, got %d", n)
	}

	// 实例已经在运行：开机请求被平台以"已在运行"拒绝，也算成功
	f.reset()
	f.alwaysRunning = true
	if _, err := c.Power(context.Background(), "ins-a", provider.PowerOn); err != nil {
		t.Errorf("已在运行时开机应视为成功: %v", err)
	}
	// 一直收不了尾：等满 opWait 后报错，不无限重试
	c.opWait = 0
	f.alwaysRunning = false
	f.busyStarts = 5
	if _, err := c.Power(context.Background(), "ins-a", provider.PowerOn); err == nil || !strings.Contains(err.Error(), "迟迟没有收尾") {
		t.Errorf("等满仍被拒应报错, got %v", err)
	}
}

// TestRebuildPlatformAutoStarts：腾讯云重装完会把实例自动开起来，不该再多发开机
// （会被"已在运行"拒绝）；重装的句柄随后即完成。
func TestRebuildPlatformAutoStarts(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("ins-a", "ap-beijing"))
	f.resetAutoStart = true
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	ref, err := c.Rebuild(context.Background(), "ins-a", "ubuntu-24.04")
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	actions := f.actions()
	i := slices.Index(actions, "ap-beijing ResetInstance")
	if i < 0 || slices.Contains(actions[i+1:], "ap-beijing StartInstances") {
		t.Errorf("平台自动开机时不应再发开机请求: %v", actions)
	}
	if st, err := c.ActionStatus(context.Background(), ref); st != "completed" || err != nil {
		t.Errorf("自动开机后重装应已完成, got %q %v", st, err)
	}
}

// TestResizeWaitsForOperationToSettle：机型先落、操作后收；收尾期间不算完成。
func TestResizeWaitsForOperationToSettle(t *testing.T) {
	f, c := seeded(t)
	inst := f.add(runningInstance("ins-a", "ap-beijing"))
	inst.state = "STOPPED"
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	ref, err := c.Resize(context.Background(), "ins-a", "SA5.MEDIUM4", false)
	if err != nil {
		t.Fatal(err)
	}
	inst.instanceType, inst.latestOp, inst.opState = "SA5.MEDIUM4", "ResetInstancesType", "OPERATING"
	inst.pending = nil
	if st, _ := c.ActionStatus(context.Background(), ref); st != "in-progress" {
		t.Errorf("机型已变但操作没收尾应为 in-progress, got %q", st)
	}
	inst.opState = "SUCCESS"
	if st, _ := c.ActionStatus(context.Background(), ref); st != "completed" {
		t.Errorf("操作收尾后应为 completed, got %q", st)
	}
}
