package ecs

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
)

// seeded 起一套假环境：青岛有默认交换机（B 区）、另一个 VPC 的默认交换机（C 区）、
// 乱序给的两个安全组、几张镜像、一把密钥；杭州有交换机但没有安全组；深圳什么都
// 没有；美国弗吉尼亚用来验证"可用区 ID 直接缀字母"的写法（us-east-1a）。
func seeded(t *testing.T) (*fakeAPI, *Client) {
	t.Helper()
	f := newFakeAPI(t)
	f.regions = []regionInfo{
		{"cn-hangzhou", "华东1（杭州）"}, {"cn-shenzhen", "华南1（深圳）"},
		{"cn-qingdao", "华北1（青岛）"}, {"us-east-1", "美国（弗吉尼亚）"},
	}
	f.zones["cn-qingdao"] = []string{"cn-qingdao-b", "cn-qingdao-c"}
	f.zones["us-east-1"] = []string{"us-east-1a", "us-east-1b"}
	f.switches["cn-qingdao"] = []vswitch{
		{VSwitchId: "vsw-c", VpcId: "vpc-1", ZoneId: "cn-qingdao-c", AvailableIpAddressCount: 900},
		{VSwitchId: "vsw-b", VpcId: "vpc-1", ZoneId: "cn-qingdao-b", IsDefault: true, AvailableIpAddressCount: 100},
		{VSwitchId: "vsw-x", VpcId: "vpc-other", ZoneId: "cn-qingdao-c", IsDefault: true, AvailableIpAddressCount: 50},
	}
	f.groups["cn-qingdao"] = []securityGroup{{"sg-2", "vpc-1"}, {"sg-1", "vpc-1"}, {"sg-9", "vpc-other"}}
	img := func(id, name, created, usage string) image {
		return image{ImageId: id, ImageName: name, OSNameEn: name, Platform: "Ubuntu", Architecture: "x86_64",
			Status: "Available", Usage: usage, CreationTime: created}
	}
	images := []image{
		img("ubuntu_24_gpu", "Ubuntu  24.04 64bit with NVIDIA GPU Driver and CUDA", "2026-11-25T00:00Z", "instance"),
		img("ubuntu_24_old", "Ubuntu  24.04 64 bit", "2026-10-01T00:00Z", "instance"),
		img("ubuntu_24_new", "Ubuntu  24.04 64 bit", "2026-11-20T00:00Z", "instance"),
		img("ubuntu_22_snap", "Ubuntu  22.04 64 bit", "2026-11-01T00:00Z", "disk"), // 只能做快照源，不能建实例
		{ImageId: "win", ImageName: "Windows Server 2022", OSNameEn: "Windows Server 2022", Platform: "Windows Server",
			Architecture: "x86_64", Status: "Available", Usage: "instance", CreationTime: "2026-11-01T00:00Z"},
	}
	for _, r := range f.regions {
		f.images[r.ID] = images
	}
	f.keyPairs["cn-qingdao"] = []keyPair{{KeyPairName: "ops", KeyPairFingerPrint: "aa:bb:00:00:00:00:00:00:00:00:00:00:00:00:00:00"}}
	f.types["cn-qingdao"] = []instanceType{
		{InstanceTypeId: "ecs.g7.large", InstanceTypeFamily: "ecs.g7", CpuCoreCount: 2, MemorySize: 8},
		{InstanceTypeId: "ecs.g8a.large", InstanceTypeFamily: "ecs.g8a", CpuCoreCount: 2, MemorySize: 8},
		{InstanceTypeId: "ecs.g8i.large", InstanceTypeFamily: "ecs.g8i", CpuCoreCount: 2, MemorySize: 8},
		{InstanceTypeId: "ecs.u1-c1m1.large", InstanceTypeFamily: "ecs.u1-c1m1", CpuCoreCount: 2, MemorySize: 2},
		{InstanceTypeId: "ecs.t5-c1m1.large", InstanceTypeFamily: "ecs.t5-c1m1", CpuCoreCount: 2, MemorySize: 2},
		{InstanceTypeId: "ecs.t6-c1m1.large", InstanceTypeFamily: "ecs.t6-c1m1", CpuCoreCount: 2, MemorySize: 2},
		{InstanceTypeId: "ecs.gn6i-c4g1.xlarge", InstanceTypeFamily: "ecs.gn6i-c4g1", CpuCoreCount: 4, MemorySize: 15, GPUAmount: 1},
		{InstanceTypeId: "ecs.d2s.6xlarge", InstanceTypeFamily: "ecs.d2s", CpuCoreCount: 24, MemorySize: 88},   // 本地盘型，不在收录系列
		{InstanceTypeId: "ecs.g8i.16xlarge", InstanceTypeFamily: "ecs.g8i", CpuCoreCount: 64, MemorySize: 256}, // 超 vCPU 上限
	}
	f.available["cn-qingdao"] = []string{
		"ecs.g7.large", "ecs.g8a.large", "ecs.g8i.large", "ecs.u1-c1m1.large", "ecs.t5-c1m1.large",
		"ecs.t6-c1m1.large", "ecs.gn6i-c4g1.xlarge", "ecs.d2s.6xlarge", "ecs.g8i.16xlarge",
	}
	f.switches["cn-hangzhou"] = []vswitch{{VSwitchId: "vsw-hz", VpcId: "vpc-hz", ZoneId: "cn-hangzhou-h"}}
	return f, f.client(t)
}

func runningInstance(id, region, zone string) fakeInstance {
	inst := fakeInstance{
		InstanceId: id, InstanceName: "node-" + id, InstanceType: "ecs.u1-c1m1.large",
		Status: "Running", ZoneId: zone, RegionId: region, Cpu: 2, Memory: 2048,
		CreationTime: "2026-11-24T08:00Z", ImageId: "ubuntu_24_new",
	}
	inst.PublicIpAddress.IpAddress = []string{"203.0.113.10"}
	inst.VpcAttributes.PrivateIpAddress.IpAddress = []string{"172.16.0.5"}
	return inst
}

// near 比较浮点月费（小时单价 × 730 有浮点误差）。
func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

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

/* ---- 纯函数 ---- */

func TestStatus(t *testing.T) {
	cases := map[string]string{
		"Running": "active", "Stopped": "off", "Pending": "new",
		"Stopping": "stopping", "Starting": "starting", "Pending:WaitStart": "pending:waitstart",
	}
	for in, want := range cases {
		if got := status(in); got != want {
			t.Errorf("status(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestParseTime(t *testing.T) {
	// ECS 的创建时间有的不带秒位
	for in, want := range map[string]string{
		"2026-11-10T11:22Z":    "2026-11-10 11:22:00 +0000 UTC",
		"2026-11-10T11:22:30Z": "2026-11-10 11:22:30 +0000 UTC",
	} {
		if got := parseTime(in); got.String() != want {
			t.Errorf("parseTime(%q) = %q, want %q", in, got.String(), want)
		}
	}
	if !parseTime("garbage").IsZero() {
		t.Error("解析不了的时间应为零值")
	}
}

func TestParseFamilyAndLatestGeneration(t *testing.T) {
	cases := map[string][3]any{
		"ecs.g8a":     {"g", 8, "a"},
		"ecs.u1-c1m1": {"u", 1, "-c1m1"},
		"ecs.e-c1m1":  {"e", 0, "-c1m1"},
		"ecs.sn1ne":   {"sn", 1, "ne"},
	}
	for family, want := range cases {
		series, gen, variant := parseFamily(family)
		if series != want[0] || gen != want[1] || variant != want[2] {
			t.Errorf("parseFamily(%s) = %q,%d,%q, want %v", family, series, gen, variant, want)
		}
	}
	if series, _, _ := parseFamily("weird"); series != "" {
		t.Error("不合命名规则的规格族应返回空系列")
	}
}

func TestImageSlugAndCatalog(t *testing.T) {
	cases := map[string]string{
		"Ubuntu  24.04 64 bit":                   "ubuntu-24.04",
		"Ubuntu  26.04 64 bit":                   "ubuntu-26.04",
		"Rocky Linux  9.8 64 bit":                "rocky-linux-9.08",
		"Alibaba Cloud Linux  3.2104 LTS 64 bit": "alibaba-cloud-linux-3.2104",
		"CentOS  7.9 64 bit":                     "centos-7.09",
		"Debian  11.0 64 bit":                    "debian-11",
	}
	for name, want := range cases {
		im := image{OSNameEn: name, Platform: strings.Fields(name)[0]}
		if strings.HasPrefix(name, "Rocky") {
			im.Platform = "Rocky Linux"
		} else if strings.HasPrefix(name, "Alibaba") {
			im.Platform = "Alibaba Cloud Linux"
		}
		if got := imageSlug(im); got != want {
			t.Errorf("imageSlug(%q) = %q, want %q", name, got, want)
		}
	}

	images := []image{
		{ImageId: "gpu", OSNameEn: "Ubuntu  24.04 64bit with NVIDIA GPU Driver and CUDA", Platform: "Ubuntu", CreationTime: "2026-11-25T00:00Z"},
		{ImageId: "old", OSNameEn: "Ubuntu  24.04 64 bit", Platform: "Ubuntu", CreationTime: "2026-10-01T00:00Z"},
		{ImageId: "new", OSNameEn: "Ubuntu  24.04 64 bit", Platform: "Ubuntu", CreationTime: "2026-11-20T00:00Z"},
	}
	bySlug := catalogImages(images)
	if got := bySlug["ubuntu-24.04"].ImageId; got != "new" {
		t.Errorf("同 slug 应选最素且最新的镜像 new, got %q", got)
	}
	// 只有带 GPU 驱动的变体时也得有一个可用项，不能凭空消失
	if got := catalogImages(images[:1])["ubuntu-24.04"].ImageId; got != "gpu" {
		t.Errorf("唯一变体应保留, got %q", got)
	}
}

func TestTagPairs(t *testing.T) {
	got := tagPairs([]string{"batch:20261124T120000Z", "bare", ":leading", "note:http://a:b"}, 20)
	want := []tagPair{{"batch", "20261124T120000Z"}, {"bare", ""}, {"note", "http://a:b"}} // ":leading" 键为空被丢弃
	if !slices.Equal(got, want) {
		t.Errorf("tagPairs = %v, want %v", got, want)
	}
	if got := tagPairs([]string{"a:1", "b:2", "c:3"}, 2); len(got) != 2 {
		t.Errorf("超出平台上限的标签应截断, got %v", got)
	}
}

func TestHostNameAndToken(t *testing.T) {
	if hostName("vps-ali-1-cn-qingdao-01") != "vps-ali-1-cn-qingdao-01" {
		t.Error("常规节点名应直接可作主机名")
	}
	for _, bad := range []string{"x", "-lead", "trail-", "a--b", "有中文", strings.Repeat("a", 61)} {
		if hostName(bad) != "" {
			t.Errorf("主机名 %q 不合法，应返回空", bad)
		}
	}
	tok := clientToken("vps-节点-with-中文")
	if len(tok) > 64 || !strings.HasPrefix(tok, "vpsctl-") || clientToken("a") == clientToken("a") {
		t.Errorf("clientToken = %q（应以 vpsctl- 开头、≤64、每次不同）", tok)
	}
}

/* ---- 列表 / 查询 ---- */

func TestListEnrichesInstances(t *testing.T) {
	f, c := seeded(t)
	a := f.add(runningInstance("i-a", "cn-qingdao", "cn-qingdao-b"))
	a.tags = map[string]string{"batch": "x", "bare": ""}
	b := f.add(runningInstance("i-b", "cn-qingdao", "cn-qingdao-b"))
	b.InstanceChargeType = prepaid // 包年包月：不问按量价，改问续费价
	b.ExpiredTime = "2026-12-10T16:00Z"
	b.autoRenew = false
	f.renewPrices["i-b"] = 1263.64
	c2 := f.add(runningInstance("i-c", "us-east-1", "us-east-1a"))
	c2.PublicIpAddress.IpAddress = nil
	c2.Status = "Stopped"
	d := f.add(runningInstance("i-d", "cn-qingdao", "cn-qingdao-b"))
	d.PublicIpAddress.IpAddress = nil
	d.EipAddress.IpAddress = "203.0.113.99" // 只绑了弹性公网 IP

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
	sa := byID["i-a"]
	if sa.Account != "acct-1" || sa.Status != "active" || sa.Region != "cn-qingdao" || sa.Size != "ecs.u1-c1m1.large" ||
		sa.VCPUs != 2 || sa.MemoryMB != 2048 || sa.DiskGB != 40 || sa.IPv4Public != "203.0.113.10" || sa.IPv4Private != "172.16.0.5" {
		t.Errorf("字段映射不符: %+v", sa)
	}
	// 询价 0.42 元/小时 → 月费 306.6，币种 CNY；镜像标注成 slug；坐标取自地域表
	if !near(sa.PriceMonthly, 306.6) || sa.Currency != "CNY" || sa.Image != "ubuntu-24.04" ||
		sa.Lat != 36.07 || sa.Lng != 120.38 || sa.CreatedAt.IsZero() {
		t.Errorf("月费/币种/镜像/坐标/时间不符: %+v", sa)
	}
	if !slices.Equal(sa.Tags, []string{"bare", "batch:x"}) { // 排过序，保证两次列表输出一致
		t.Errorf("标签应还原成 key:value（空值的只留键）: %v", sa.Tags)
	}
	// 按量节点另外带出真实计费单位：每小时单价；没有到期时间、没有自动续费这一说
	if sa.Billing != provider.BillingHourly || !near(sa.PriceHourly, 0.42) ||
		!sa.ExpiresAt.IsZero() || sa.AutoRenew != nil {
		t.Errorf("按量实例的计费方式 / 小时价 / 到期 / 自动续费不符: %+v", sa)
	}
	// 包年包月：月价是"续一个月要多少钱"，带到期时间与自动续费状态，不带小时价
	exp := time.Date(2026, 12, 10, 16, 0, 0, 0, time.UTC)
	sb := byID["i-b"]
	if sb.Billing != provider.BillingMonthly || !near(sb.PriceMonthly, 1263.64) || sb.PriceHourly != 0 ||
		!sb.ExpiresAt.Equal(exp) || sb.AutoRenew == nil || *sb.AutoRenew {
		t.Errorf("包年包月实例的计费 / 续费价 / 到期 / 自动续费不符: %+v", sb)
	}
	if sc := byID["i-c"]; sc.Status != "off" || sc.IPv4Public != "" || sc.Lat != 38.91 {
		t.Errorf("已关机实例映射不符: %+v", sc)
	}
	if sd := byID["i-d"]; sd.IPv4Public != "203.0.113.99" {
		t.Errorf("没有普通公网 IP 时应回退到弹性公网 IP: %+v", sd)
	}
	// 询价：按量实例按"可用区|规格|盘"去重后各问一次（青岛、美国各一档），包年包月的不问
	if n := countAction(f, "DescribePrice"); n != 2 {
		t.Errorf("DescribePrice 应调 2 次, got %d", n)
	}
	// 续费价与自动续费状态都逐台问（只问包年包月的）
	if n := countAction(f, "DescribeRenewalPrice"); n != 1 {
		t.Errorf("DescribeRenewalPrice 应调 1 次, got %d", n)
	}
	if p := f.lastParams("DescribeRenewalPrice"); p["RegionId"] != "cn-qingdao" || p["ResourceId"] != "i-b" ||
		p["Period"] != "" || p["PriceUnit"] != "" {
		t.Errorf("续费价只给必填参数（= 续 1 个月）: %v", p)
	}
	if n := countAction(f, "DescribeInstanceAutoRenewAttribute"); n != 1 {
		t.Errorf("DescribeInstanceAutoRenewAttribute 应调 1 次, got %d", n)
	}
	// 平台要求带实例 ID：不带的话整台实例的自动续费状态就查不出来（上面的断言会红）
	if p := f.lastParams("DescribeInstanceAutoRenewAttribute"); p["RegionId"] != "cn-qingdao" || p["InstanceId"] != "i-b" {
		t.Errorf("自动续费状态应按实例查: %v", p)
	}
	// 标签按地域分批查，资源 ID 平铺成 ResourceId.N
	tagged := 0
	for _, p := range f.paramsOf("ListTagResources") {
		if p["ResourceType"] != "instance" {
			t.Errorf("ListTagResources ResourceType 不符: %v", p)
		}
		if p["ResourceId.1"] != "" {
			tagged++
		}
	}
	if tagged != 2 { // 青岛 3 台同批、美国 1 台一批
		t.Errorf("ListTagResources 应按地域各查 1 次, got %d", tagged)
	}
	if _, err := c.List(context.Background()); err != nil || countAction(f, "DescribeRegions") != 1 {
		t.Errorf("地域表应只取 1 次: err=%v calls=%d", err, countAction(f, "DescribeRegions"))
	}
	// 续费价进缓存：再列一次不重打（自动续费状态可能被人在控制台改，每次都现查）
	if n := countAction(f, "DescribeRenewalPrice"); n != 1 {
		t.Errorf("续费价应缓存, DescribeRenewalPrice 仍应是 1 次, got %d", n)
	}
	if n := countAction(f, "DescribeInstanceAutoRenewAttribute"); n != 2 {
		t.Errorf("自动续费状态不应缓存, DescribeInstanceAutoRenewAttribute 应为 2 次, got %d", n)
	}
}

// 续费价或自动续费属性问不到，不该拖垮整张列表：节点照常列出，只是相应的那项留空。
func TestPrepaidInfoFailuresDoNotBreakList(t *testing.T) {
	f, c := seeded(t)
	b := f.add(runningInstance("i-b", "cn-qingdao", "cn-qingdao-b"))
	b.InstanceChargeType = prepaid
	b.ExpiredTime = "2026-12-10T16:00Z"
	f.failRenew, f.failAutoRenew = true, true

	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("包年包月的补充信息问不到不应让列表失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("共 %d 台, want 1: %+v", len(got), got)
	}
	s := got[0]
	if s.Billing != provider.BillingMonthly || s.PriceMonthly != 0 || s.AutoRenew != nil {
		t.Errorf("问不到时应留空而不是报错或编一个数: %+v", s)
	}
	if s.ExpiresAt.IsZero() {
		t.Errorf("到期时间来自实例列表本身，不该受询价失败影响: %+v", s)
	}
	// 问不到也缓存，免得每次列表都重打
	if _, err := c.List(context.Background()); err != nil || countAction(f, "DescribeRenewalPrice") != 1 {
		t.Errorf("失败的询价也应缓存: err=%v calls=%d", err, countAction(f, "DescribeRenewalPrice"))
	}
}

// 地域里没有包年包月实例时不该去问自动续费属性。
func TestNoAutoRenewQueryWithoutPrepaid(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("i-a", "cn-qingdao", "cn-qingdao-b"))
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, a := range f.actions() {
		if strings.HasSuffix(a, " DescribeRenewalPrice") || strings.HasSuffix(a, " DescribeInstanceAutoRenewAttribute") {
			t.Errorf("没有包年包月实例不该调续费相关接口: %s", a)
		}
	}
}

func TestListRegionFailureFailsWholeList(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("i-a", "cn-qingdao", "cn-qingdao-b"))
	f.failList["cn-shenzhen"] = true
	if _, err := c.List(context.Background()); err == nil || !strings.Contains(err.Error(), "cn-shenzhen") {
		t.Errorf("任一地域失败应整体失败并点名地域, got %v", err)
	}
}

// TestGetLocatesWhenNotIndexed：进程刚起、没 List 过就按 ID 操作，应逐地域并发
// 找到并登记，之后不再扫。
func TestGetLocatesWhenNotIndexed(t *testing.T) {
	f, c := seeded(t)
	inst := f.add(runningInstance("i-a", "cn-qingdao", "cn-qingdao-b"))
	inst.tags = map[string]string{"k": "v"}

	s, err := c.Get(context.Background(), "i-a")
	if err != nil || s.ID != "i-a" || s.Region != "cn-qingdao" || !slices.Equal(s.Tags, []string{"k:v"}) {
		t.Fatalf("Get = %+v, %v", s, err)
	}
	f.reset()
	if _, err := c.Get(context.Background(), "i-a"); err != nil {
		t.Fatal(err)
	}
	if n := countAction(f, "DescribeInstances"); n != 1 {
		t.Errorf("找到后应登记索引，第二次 Get 只查 1 个地域, got %d 次: %v", n, f.actions())
	}
}

func TestGetNotFound(t *testing.T) {
	f, c := seeded(t)
	if _, err := c.Get(context.Background(), "i-none"); !errors.Is(err, errNotFound) {
		t.Errorf("不存在的实例应报 errNotFound, got %v", err)
	}
	if n := countAction(f, "DescribeInstances"); n != len(f.regions) {
		t.Errorf("应查遍全部 %d 个地域, got %d", len(f.regions), n)
	}
}

func TestResolvePlacement(t *testing.T) {
	_, c := seeded(t)
	ctx := context.Background()
	cases := map[string][2]string{
		"cn-qingdao":   {"cn-qingdao", ""},
		"cn-qingdao-c": {"cn-qingdao", "cn-qingdao-c"},
		"us-east-1a":   {"us-east-1", "us-east-1a"}, // 可用区 ID 直接缀字母
	}
	for slug, want := range cases {
		region, zone, err := c.resolvePlacement(ctx, slug)
		if err != nil || region != want[0] || zone != want[1] {
			t.Errorf("resolvePlacement(%q) = %q,%q %v, want %v", slug, region, zone, err, want)
		}
	}
	if _, _, err := c.resolvePlacement(ctx, "cn-qingdao-z"); err == nil || !strings.Contains(err.Error(), "cn-qingdao-z") {
		t.Errorf("不存在的可用区应报错, got %v", err)
	}
}

/* ---- 电源 / 动作状态 / 销毁 ---- */

func TestPowerAndActionStatus(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("i-a", "cn-qingdao", "cn-qingdao-b"))
	ctx := context.Background()
	if _, err := c.List(ctx); err != nil {
		t.Fatal(err)
	}

	// 优雅关机：不强关，普通停机模式（保住公网 IP）
	ref, err := c.Power(ctx, "i-a", provider.Shutdown)
	if err != nil {
		t.Fatalf("Power: %v", err)
	}
	if ref.ID != "cn-qingdao|i-a|shutdown|" {
		t.Errorf("句柄 = %q", ref.ID)
	}
	if p := f.lastParams("StopInstance"); p["ForceStop"] != "false" || p["StoppedMode"] != "KeepCharging" {
		t.Errorf("优雅关机应为 ForceStop=false + KeepCharging: %v", p)
	}
	if st, _ := c.ActionStatus(ctx, ref); st != "in-progress" {
		t.Errorf("关机执行中应为 in-progress, got %q", st)
	}
	if st, err := c.ActionStatus(ctx, ref); st != "completed" || err != nil {
		t.Errorf("关机完成后应为 completed, got %q %v", st, err)
	}
	if p := f.lastParams("StopInstance"); p["ForceStop"] != "false" {
		t.Errorf("应打到地域专属接入点且参数正确: %v", p)
	}

	if _, err := c.Power(ctx, "i-a", provider.PowerOff); err != nil {
		t.Fatal(err)
	}
	if p := f.lastParams("StopInstance"); p["ForceStop"] != "true" {
		t.Errorf("power_off 应为 ForceStop=true: %v", p)
	}
	ref, err = c.Power(ctx, "i-a", provider.PowerOn)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := c.ActionStatus(ctx, ref); st != "in-progress" {
		t.Errorf("开机执行中应为 in-progress, got %q", st)
	}
	if st, _ := c.ActionStatus(ctx, ref); st != "completed" {
		t.Errorf("开机完成后应为 completed, got %q", st)
	}
	if _, err := c.Power(ctx, "i-a", "reboot"); err == nil {
		t.Error("不支持的动作应报错")
	}
	if _, err := c.ActionStatus(ctx, provider.ActionRef{ID: "cn-qingdao|i-a|nope|"}); err == nil {
		t.Error("未知动作种类应报错")
	}
}

func TestDelete(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("i-a", "cn-qingdao", "cn-qingdao-b"))
	f.add(runningInstance("i-b", "us-east-1", "us-east-1a"))
	ctx := context.Background()
	if err := c.Delete(ctx, "i-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if f.get("i-a") != nil || f.lastParams("DeleteInstance")["Force"] != "true" {
		t.Errorf("实例未被释放，或没允许释放运行中的实例")
	}
	// 已不存在视为成功（批量重试幂等）；未登记过的 ID 也能自己找到地域
	if err := c.Delete(ctx, "i-a"); err != nil {
		t.Errorf("释放已不存在的实例应视为成功, got %v", err)
	}
	if err := c.Delete(ctx, "i-b"); err != nil || f.get("i-b") != nil {
		t.Errorf("未登记索引的实例应能定位并释放: %v", err)
	}
}

/* ---- 建机 ---- */

func TestCreate(t *testing.T) {
	f, c := seeded(t)
	f.diskOK = map[string]bool{"cloud_essd_entry": true} // ESSD 被拒 → 顺延到 ESSD Entry
	userData := "#cloud-config\nssh_pwauth: true\n"

	s, err := c.Create(context.Background(), provider.CreateRequest{
		Name: "vps-ali-1-cn-qingdao-01", Region: "cn-qingdao", Size: "ecs.u1-c1m1.large", Image: "ubuntu-24.04",
		Tags: []string{"batch:20261124T120000Z", "bare"}, UserData: userData, SSHKeys: []string{"ops"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.ID != "i-test0001" || s.Status != "new" || s.Region != "cn-qingdao" || s.Currency != "CNY" ||
		s.IPv4Public != "203.0.113.1" || !near(s.PriceMonthly, 306.6) || s.Image != "ubuntu-24.04" {
		t.Errorf("返回的节点不符: %+v", s)
	}

	var dry, real map[string]string
	dryRuns := 0
	for _, p := range f.paramsOf("RunInstances") {
		if p["DryRun"] == "true" {
			dryRuns++
			dry = p
		} else {
			real = p
		}
	}
	if dryRuns != 2 || real == nil {
		t.Fatalf("应预检 2 种云盘类型再真建 1 次: 预检 %d 次", dryRuns)
	}
	if _, has := dry["ClientToken"]; has {
		t.Error("预检不应带幂等令牌（与正式建机共用会撞）")
	}
	if tok := real["ClientToken"]; tok == "" || len(tok) > 64 {
		t.Errorf("正式建机应带 ≤64 字符的幂等令牌, got %q", tok)
	}
	if real["InstanceChargeType"] != postpaid || real["InternetChargeType"] != "PayByTraffic" {
		t.Errorf("应按量付费 + 按流量计费: %v", real)
	}
	if real["ImageId"] != "ubuntu_24_new" {
		t.Errorf("ubuntu-24.04 应解析成最新素镜像 ubuntu_24_new, got %v", real["ImageId"])
	}
	// 未指定可用区 → 落在默认交换机所在的 B 区；安全组取同 VPC 里 ID 最小的
	if real["ZoneId"] != "cn-qingdao-b" || real["VSwitchId"] != "vsw-b" || real["SecurityGroupId"] != "sg-1" {
		t.Errorf("网络选择不符: zone=%v vsw=%v sg=%v", real["ZoneId"], real["VSwitchId"], real["SecurityGroupId"])
	}
	// 云盘类型：预检被拒的 ESSD 不能用在正式请求里
	if real["SystemDisk.Category"] != "cloud_essd_entry" || real["SystemDisk.Size"] != "40" {
		t.Errorf("系统盘 = %v %v", real["SystemDisk.Category"], real["SystemDisk.Size"])
	}
	if real["KeyPairName"] != "ops" {
		t.Errorf("KeyPairName = %v", real["KeyPairName"])
	}
	ud, _ := real["UserData"]
	if decoded, err := base64.StdEncoding.DecodeString(ud); err != nil || string(decoded) != userData {
		t.Errorf("UserData 应 base64 编码原文, got %q (%v)", ud, err)
	}
	if real["Tag.1.Key"] != "batch" || real["Tag.1.Value"] != "20261124T120000Z" ||
		real["Tag.2.Key"] != "bare" || real["Tag.2.Value"] != "" {
		t.Errorf("标签应按 Tag.N.Key / Tag.N.Value 平铺: %v", real)
	}
	if real["HostName"] != "vps-ali-1-cn-qingdao-01" {
		t.Errorf("HostName = %v", real["HostName"])
	}
}

// TestCreateExplicitZone：区域 slug 给成可用区时按该可用区选交换机。
func TestCreateExplicitZone(t *testing.T) {
	f, c := seeded(t)
	if _, err := c.Create(context.Background(), provider.CreateRequest{
		Name: "node-01", Region: "cn-qingdao-c", Size: "ecs.u1-c1m1.large", Image: "ubuntu_24_old",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	p := f.lastParams("RunInstances")
	if p["ZoneId"] != "cn-qingdao-c" || p["VSwitchId"] != "vsw-x" {
		t.Errorf("应选 cn-qingdao-c 的默认交换机, got zone=%v vsw=%v", p["ZoneId"], p["VSwitchId"])
	}
	if p["SecurityGroupId"] != "sg-9" {
		t.Errorf("安全组应取交换机所在 VPC 的: %v", p["SecurityGroupId"])
	}
	if p["ImageId"] != "ubuntu_24_old" {
		t.Error("直接的 ImageId 应原样使用")
	}
}

func TestCreateErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("地域没有交换机", func(t *testing.T) {
		f, c := seeded(t)
		_, err := c.Create(ctx, provider.CreateRequest{Name: "node-01", Region: "cn-shenzhen", Size: "ecs.u1-c1m1.large", Image: "ubuntu_24_old"})
		if err == nil || !strings.Contains(err.Error(), "交换机") {
			t.Errorf("应提示先建交换机, got %v", err)
		}
		if countAction(f, "RunInstances") != 0 {
			t.Error("网络没准备好就不该发建机请求")
		}
	})
	t.Run("VPC 没有安全组", func(t *testing.T) {
		_, c := seeded(t)
		_, err := c.Create(ctx, provider.CreateRequest{Name: "node-01", Region: "cn-hangzhou", Size: "ecs.u1-c1m1.large", Image: "ubuntu_24_old"})
		if err == nil || !strings.Contains(err.Error(), "安全组") {
			t.Errorf("应提示先建安全组, got %v", err)
		}
	})
	t.Run("预检全不通过", func(t *testing.T) {
		f, c := seeded(t)
		f.diskOK = map[string]bool{}
		_, err := c.Create(ctx, provider.CreateRequest{Name: "node-01", Region: "cn-qingdao", Size: "ecs.t5-c1m1.large", Image: "ubuntu_24_old"})
		if err == nil || !strings.Contains(err.Error(), "DryRun") || !strings.Contains(err.Error(), "NotSupportDiskCategory") {
			t.Errorf("应带出最后一次预检的原始错误, got %v", err)
		}
		if len(f.instances) != 0 || countAction(f, "RunInstances") != len(diskCandidates) {
			t.Errorf("预检失败不应真建机；预检次数 = %d", countAction(f, "RunInstances"))
		}
	})
	t.Run("区域不存在", func(t *testing.T) {
		_, c := seeded(t)
		_, err := c.Create(ctx, provider.CreateRequest{Name: "node-01", Region: "cn-mars", Size: "ecs.u1-c1m1.large", Image: "ubuntu_24_old"})
		if err == nil || !strings.Contains(err.Error(), "cn-mars") {
			t.Errorf("应报未知区域, got %v", err)
		}
	})
	t.Run("镜像不存在", func(t *testing.T) {
		_, c := seeded(t)
		_, err := c.Create(ctx, provider.CreateRequest{Name: "node-01", Region: "cn-qingdao", Size: "ecs.u1-c1m1.large", Image: "ubuntu-99.04"})
		if err == nil || !strings.Contains(err.Error(), "ubuntu-24.04") {
			t.Errorf("应报错并列出可用镜像, got %v", err)
		}
	})
	t.Run("密钥对不存在", func(t *testing.T) {
		f, c := seeded(t)
		_, err := c.Create(ctx, provider.CreateRequest{Name: "node-01", Region: "cn-qingdao", Size: "ecs.u1-c1m1.large", Image: "ubuntu_24_old", SSHKeys: []string{"nope"}})
		if err == nil || !strings.Contains(err.Error(), "ops") {
			t.Errorf("应报错并列出可用密钥对, got %v", err)
		}
		if countAction(f, "RunInstances") != 0 {
			t.Error("密钥对解析失败就不该发建机请求")
		}
	})
}

/* ---- 重装 / 改配 ---- */

func TestRebuildRunningInstance(t *testing.T) {
	f, c := seeded(t)
	inst := f.add(runningInstance("i-a", "cn-qingdao", "cn-qingdao-b"))
	f.replacePolls = 3 // 换盘要三次查询才结束：验证会等它真的结束再开机
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref, err := c.Rebuild(ctx, "i-a", "ubuntu-24.04")
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	actions := f.actions()
	i := slices.Index(actions, "cn-qingdao StopInstance")
	j := slices.Index(actions, "cn-qingdao ReplaceSystemDisk")
	k := slices.Index(actions, "cn-qingdao StartInstance")
	if i < 0 || j < i || k < j {
		t.Fatalf("应按 关机 → 换盘 → 开机 的顺序: %v", actions)
	}
	// 换盘结束前至少查了 3 次，不能抢跑开机
	queries := 0
	for _, a := range actions[j:k] {
		if a == "cn-qingdao DescribeInstances" {
			queries++
		}
	}
	if queries < 3 {
		t.Errorf("开机前应至少查询换盘进度 3 次, got %d: %v", queries, actions[j:k])
	}
	if p := f.lastParams("ReplaceSystemDisk"); p["InstanceId"] != "i-a" || p["ImageId"] != "ubuntu_24_new" {
		t.Errorf("换盘请求不符: %v", p)
	}
	if ref.ID != "cn-qingdao|i-a|rebuild|" {
		t.Errorf("应返回开机的句柄: %q", ref.ID)
	}
	if st, _ := c.ActionStatus(ctx, ref); st != "in-progress" {
		t.Errorf("开机中应为 in-progress, got %q", st)
	}
	if st, _ := c.ActionStatus(ctx, ref); st != "completed" {
		t.Errorf("开机完成应为 completed, got %q", st)
	}
	if inst.ImageId != "ubuntu_24_new" || inst.Status != "Running" {
		t.Errorf("重装后镜像应更新、实例应回到运行中: %+v", inst)
	}
	// 系统盘被换掉了：缓存的旧盘信息要作废
	if _, ok := c.disks.Get("cn-qingdao|i-a"); ok {
		t.Error("换盘后旧的系统盘缓存应作废")
	}
}

func TestRebuildStoppedInstanceSkipsPowerOff(t *testing.T) {
	f, c := seeded(t)
	inst := f.add(runningInstance("i-a", "cn-qingdao", "cn-qingdao-b"))
	inst.Status = "Stopped"
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Rebuild(context.Background(), "i-a", "ubuntu-24.04"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	actions := f.actions()
	if slices.Contains(actions, "cn-qingdao StopInstance") || !slices.Contains(actions, "cn-qingdao StartInstance") {
		t.Errorf("应只 换盘 → 开机: %v", actions)
	}
}

func TestResize(t *testing.T) {
	f, c := seeded(t)
	inst := f.add(runningInstance("i-a", "cn-qingdao", "cn-qingdao-b"))
	inst.Status = "Stopped"
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Resize(ctx, "i-a", "ecs.u1-c1m2.large", true); err == nil || !strings.Contains(err.Error(), "扩磁盘") {
		t.Errorf("扩盘请求应明确拒绝, got %v", err)
	}
	if countAction(f, "ModifyInstanceSpec") != 0 {
		t.Error("拒绝的改配不应下发")
	}
	ref, err := c.Resize(ctx, "i-a", "ecs.u1-c1m2.large", false)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if ref.ID != "cn-qingdao|i-a|resize|ecs.u1-c1m2.large" {
		t.Errorf("句柄 = %q", ref.ID)
	}
	if p := f.lastParams("ModifyInstanceSpec"); p["InstanceId"] != "i-a" || p["InstanceType"] != "ecs.u1-c1m2.large" {
		t.Errorf("改配请求不符: %v", p)
	}
	// 同步接口：规格已变即完成
	if st, _ := c.ActionStatus(ctx, ref); st != "completed" {
		t.Errorf("规格已变应为 completed, got %q", st)
	}
	if inst.InstanceType != "ecs.u1-c1m2.large" {
		t.Errorf("规格未变更: %q", inst.InstanceType)
	}
}

/* ---- 目录 ---- */

func TestCatalogs(t *testing.T) {
	f, c := seeded(t)
	f.add(runningInstance("i-a", "cn-qingdao", "cn-qingdao-b"))
	ctx := context.Background()

	regions, err := c.Regions(ctx)
	if err != nil || len(regions) != 4 || regions[0].Slug != "cn-hangzhou" || regions[0].Name != "华东1（杭州）" {
		t.Errorf("Regions = %+v, %v", regions, err)
	}

	// 目录地域 = 节点最多的地域（青岛），镜像 / 规格 / 密钥都从那里取
	images, err := c.Images(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var slugs []string
	for _, im := range images {
		slugs = append(slugs, im.Slug)
	}
	// Windows 与只能做快照源的镜像不进目录；GPU 变体让位给素镜像
	if want := []string{"ubuntu-24.04"}; !slices.Equal(slugs, want) {
		t.Errorf("镜像目录 = %v, want %v", slugs, want)
	}

	sizes, err := c.Sizes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range sizes {
		got = append(got, s.Slug)
	}
	// 只收在售、收录系列、最新一代、无 GPU、≤8 vCPU 的规格；按 vCPU → 内存 → 名称排序
	// 同代里不同后缀的变体（g8a / g8i）与没有更新一代的基础系（g7）都各自保留
	want := []string{"ecs.t6-c1m1.large", "ecs.u1-c1m1.large", "ecs.g7.large", "ecs.g8a.large", "ecs.g8i.large"}
	if !slices.Equal(got, want) {
		t.Errorf("sizes = %v, want %v", got, want)
	}
	if sizes[0].MemoryMB != 2048 || sizes[0].Currency != "CNY" || sizes[0].PriceMonthly != 0 {
		t.Errorf("规格目录不带月费、币种 CNY: %+v", sizes[0])
	}

	keys, err := c.SSHKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].ID != "ops" || keys[0].Fingerprint != "aa:bb:00:00:00:00:00:00:00:00:00:00:00:00:00:00" {
		t.Errorf("SSHKeys = %+v, %v", keys, err)
	}
}

// TestRebuildRejectedRestoresPower：平台拒绝换盘时系统盘没动，为了重装才关掉的机要开
// 回去，错误里带出平台原因；本来就已关机的机则不动电源。
func TestRebuildRejectedRestoresPower(t *testing.T) {
	f, c := seeded(t)
	inst := f.add(runningInstance("i-a", "cn-qingdao", "cn-qingdao-b"))
	f.replaceReject = true
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.Rebuild(context.Background(), "i-a", "ubuntu-24.04")
	if err == nil || !strings.Contains(err.Error(), "TargetImageIncompatible") || !strings.Contains(err.Error(), "已发起开机") {
		t.Fatalf("应带出平台原因与恢复结果, got %v", err)
	}
	actions := f.actions()
	if i, j := slices.Index(actions, "cn-qingdao ReplaceSystemDisk"), slices.Index(actions, "cn-qingdao StartInstance"); i < 0 || j < i {
		t.Fatalf("换盘被拒后应发起开机恢复: %v", actions)
	}
	if inst.ImageId != "ubuntu_24_new" {
		t.Errorf("被拒的重装不应改动系统盘: %q", inst.ImageId)
	}

	inst.Status, inst.pending = "Stopped", nil
	f.reset()
	if _, err := c.Rebuild(context.Background(), "i-a", "ubuntu-24.04"); err == nil {
		t.Fatal("重装被拒应报错")
	}
	if actions := f.actions(); slices.Contains(actions, "cn-qingdao StartInstance") {
		t.Errorf("已关机的实例重装被拒，不应动电源: %v", actions)
	}
}
