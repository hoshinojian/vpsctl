package lighthouse

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeAPI 模拟腾讯云轻量应用服务器的 API：按 X-TC-Action 与 X-TC-Region 分发，维护一张
// 测试可改动的实例表。异步接口不立刻改状态，而是登记一个过渡，之后第 N 次查询该
// 实例时才落到终态。
type fakeAPI struct {
	t *testing.T

	mu             sync.Mutex
	regions        []string
	zones          map[string][]string
	instances      map[string]*fakeInstance
	blueprints     map[string][]blueprint
	bundles        map[string][]bundle
	keyPairs       map[string][]keyPair
	calls          []string                  // "地域 动作"序列
	bodies         map[string]map[string]any // 建机预检 / 真建最近一次的请求体
	nextID         int
	lastIDsOf      map[string][]any // 各动作最近一次请求里的 InstanceIds
	failList       map[string]bool  // 这些地域的 DescribeInstances 报错
	dryRunCode     string           // 非空时 CreateInstances 的 DryRun 返回这个错误码
	resetReject    bool
	resetAutoStart bool
	isolateReject  bool
	busyStarts     int // 接下来这么多次开机请求被"操作进行中"拒绝
}

type fakeInstance struct {
	id, region, zone, name, state, bundle, platform, osName, publicIP, expired, renew string
	tags                                                                              []tag
	latestOp, opState, opErr                                                          string

	pending *pendingOp
}

type pendingOp struct {
	pollsLeft int
	final     func(*fakeInstance)
}

func newFakeAPI(t *testing.T) *fakeAPI {
	return &fakeAPI{
		t: t, instances: map[string]*fakeInstance{}, zones: map[string][]string{},
		blueprints: map[string][]blueprint{}, bundles: map[string][]bundle{}, keyPairs: map[string][]keyPair{},
		bodies: map[string]map[string]any{}, failList: map[string]bool{}, lastIDsOf: map[string][]any{},
	}
}

// seeded 起一套假环境：广州、北京两个地域，广州有系统镜像与应用镜像各两张、三个套餐、
// 一把密钥；北京只有镜像。
func seeded(t *testing.T) (*fakeAPI, *Client) {
	t.Helper()
	f := newFakeAPI(t)
	f.regions = []string{"ap-beijing", "ap-guangzhou"}
	f.zones["ap-guangzhou"] = []string{"ap-guangzhou-3", "ap-guangzhou-4"}
	osBp := func(id, platform, osName string) blueprint {
		return blueprint{BlueprintId: id, Platform: platform, PlatformType: "LINUX_UNIX", OsName: osName,
			BlueprintType: "PURE_OS", BlueprintState: "NORMAL"}
	}
	f.blueprints["ap-guangzhou"] = []blueprint{
		osBp("lhbp-u24", "UBUNTU", "Ubuntu Server 24.04 LTS 64bit"),
		osBp("lhbp-u22", "UBUNTU", "Ubuntu Server 22.04 LTS 64bit"),
		osBp("lhbp-deb12", "DEBIAN", "Debian 12.0 64bit"),
		osBp("lhbp-deb13", "DEBIAN", "Debian 13.2 64bit"),
		{BlueprintId: "lhbp-app", Platform: "UBUNTU", PlatformType: "LINUX_UNIX", OsName: "Ubuntu Server 24.04 LTS 64bit",
			BlueprintType: "APP_OS", BlueprintState: "NORMAL"}, // 预装应用的镜像
		{BlueprintId: "lhbp-docker", Platform: "UBUNTU", PlatformType: "LINUX_UNIX", OsName: "Ubuntu Server 24.04 LTS 64bit",
			BlueprintType: "DOCKER", BlueprintState: "NORMAL"},
	}
	f.blueprints["ap-beijing"] = f.blueprints["ap-guangzhou"]
	price := func(p float64) bundle {
		b := bundle{}
		b.Price.InstancePrice.Currency, b.Price.InstancePrice.DiscountPrice, b.Price.InstancePrice.OriginalPrice = "CNY", p, p
		return b
	}
	f.bundles["ap-guangzhou"] = []bundle{
		func() bundle {
			b := price(55)
			b.BundleId, b.CPU, b.Memory, b.SystemDiskSize, b.BundleSalesState = "bundle_big", 4, 4, 60, "AVAILABLE"
			return b
		}(),
		func() bundle {
			b := price(35)
			b.BundleId, b.CPU, b.Memory, b.SystemDiskSize, b.BundleSalesState, b.SupportLinuxUnixPlatform = "bundle_small", 2, 2, 40, "AVAILABLE", true
			return b
		}(),
		func() bundle {
			b := price(45)
			b.BundleId, b.CPU, b.Memory, b.SystemDiskSize, b.BundleSalesState = "bundle_soldout", 2, 2, 40, "SOLD_OUT"
			return b
		}(),
	}
	f.keyPairs["ap-guangzhou"] = []keyPair{{KeyPairId: "lhkp-1", KeyName: "ops", PublicKey: "ssh-rsa AAAA ops@example"}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New("acct-1", "AKIDtest", "test-secret", srv.Client())
	c.api.SetEndpoint(srv.URL)
	c.sleep = func(context.Context, time.Duration) error { return nil }
	c.pollEvery = time.Millisecond
	return f, c
}

func (f *fakeAPI) add(inst fakeInstance) *fakeInstance {
	f.instances[inst.id] = &inst
	return &inst
}

func (f *fakeAPI) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	out = append(out, f.calls...)
	return out
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	action, region := r.Header.Get("X-TC-Action"), r.Header.Get("X-TC-Region")
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, region+" "+action)
	if action == "CreateInstances" {
		dry, _ := body["DryRun"].(bool)
		f.bodies[dryKey(dry)] = body
	}
	if ids, ok := body["InstanceIds"].([]any); ok {
		f.lastIDsOf[action] = ids
	}
	if r.Header.Get("Authorization") == "" || r.Host != host {
		f.t.Errorf("请求 %s 缺 Authorization 或 Host 不对: %q", action, r.Host)
	}

	switch action {
	case "DescribeRegions":
		set := make([]map[string]any, 0, len(f.regions))
		for _, reg := range f.regions {
			set = append(set, map[string]any{"Region": reg, "RegionName": reg + "区"})
		}
		f.write(w, map[string]any{"RegionSet": set})
	case "DescribeZones":
		set := []map[string]any{}
		for _, z := range f.zones[region] {
			set = append(set, map[string]any{"Zone": z})
		}
		f.write(w, map[string]any{"ZoneInfoSet": set})
	case "DescribeInstances":
		if f.failList[region] {
			f.fail(w, "FailedOperation", "boom")
			return
		}
		set := []map[string]any{}
		for _, inst := range f.instances {
			if inst.region != region {
				continue
			}
			if inst.pending != nil {
				inst.pending.pollsLeft--
				if inst.pending.pollsLeft <= 0 {
					inst.pending.final(inst)
					inst.pending = nil
				}
			}
			set = append(set, inst.json())
		}
		f.write(w, map[string]any{"TotalCount": len(set), "InstanceSet": set})
	case "DescribeBundles":
		f.write(w, map[string]any{"BundleSet": f.bundles[region]})
	case "DescribeKeyPairs":
		f.write(w, map[string]any{"KeyPairSet": f.keyPairs[region]})
	case "DescribeBlueprints":
		f.write(w, map[string]any{"TotalCount": len(f.blueprints[region]), "BlueprintSet": f.blueprints[region]})
	case "CreateInstances":
		if dry, _ := body["DryRun"].(bool); dry {
			if f.dryRunCode != "" {
				f.fail(w, f.dryRunCode, "dry run rejected")
				return
			}
			f.write(w, map[string]any{})
			return
		}
		f.nextID++
		id := "lhins-test" + string(rune('0'+f.nextID))
		inst := &fakeInstance{id: id, region: region, state: "PENDING",
			name: body["InstanceName"].(string), bundle: body["BundleId"].(string),
			platform: "UBUNTU", osName: "Ubuntu Server 24.04 LTS 64bit", publicIP: "203.0.113." + string(rune('0'+f.nextID)),
			expired: "2026-12-24T12:00:00Z"}
		f.instances[id] = inst
		f.write(w, map[string]any{"InstanceIdSet": []string{id}})
	case "StartInstances":
		if f.busyStarts > 0 {
			f.busyStarts--
			f.fail(w, "OperationDenied.InstanceOperationInProgress", "operation in progress")
			return
		}
		f.each(body, func(inst *fakeInstance) {
			inst.latestOp, inst.opState = "StartInstances", "OPERATING"
			inst.state = "STARTING"
			inst.pending = &pendingOp{2, func(i *fakeInstance) { i.state, i.opState = "RUNNING", "SUCCESS" }}
		})
		f.write(w, map[string]any{})
	case "StopInstances":
		f.each(body, func(inst *fakeInstance) {
			inst.latestOp, inst.opState = "StopInstances", "OPERATING"
			inst.state = "STOPPING"
			inst.pending = &pendingOp{2, func(i *fakeInstance) { i.state, i.opState = "STOPPED", "SUCCESS" }}
		})
		f.write(w, map[string]any{})
	case "IsolateInstances":
		if f.isolateReject {
			f.fail(w, "UnsupportedOperation.ResourceNotReturnable", "resource not returnable")
			return
		}
		f.each(body, func(inst *fakeInstance) {
			inst.latestOp, inst.opState = "IsolateInstances", "OPERATING"
			inst.pending = &pendingOp{2, func(i *fakeInstance) { i.state, i.opState = "SHUTDOWN", "SUCCESS" }}
		})
		f.write(w, map[string]any{})
	case "TerminateInstances":
		// 平台只接受 SHUTDOWN 状态的实例：运行中 / 已关机的都拒绝（真机验证时踩到的语义）
		var bad, badState string
		f.each(body, func(inst *fakeInstance) {
			if inst.state != "SHUTDOWN" && bad == "" {
				bad, badState = inst.id, inst.state
			}
		})
		if bad != "" {
			f.fail(w, "UnsupportedOperation.InvalidInstanceState",
				"The instance `"+bad+"` is in `"+badState+"` state and does not support this operation.")
			return
		}
		f.each(body, func(inst *fakeInstance) { delete(f.instances, inst.id) })
		f.write(w, map[string]any{})
	case "ResetInstance":
		if f.resetReject {
			f.fail(w, "UnsupportedOperation", "reset rejected")
			return
		}
		f.each(body, func(inst *fakeInstance) {
			inst.latestOp, inst.opState = "ResetInstance", "OPERATING"
			inst.pending = &pendingOp{3, func(i *fakeInstance) {
				i.opState = "SUCCESS"
				if f.resetAutoStart {
					i.state = "RUNNING"
				}
			}}
		})
		f.write(w, map[string]any{})
	case "ModifyInstancesBundle":
		f.each(body, func(inst *fakeInstance) {
			inst.latestOp, inst.opState = "ModifyInstancesBundle", "OPERATING"
			inst.pending = &pendingOp{2, func(i *fakeInstance) {
				i.bundle, i.opState = body["BundleId"].(string), "SUCCESS"
			}}
		})
		f.write(w, map[string]any{})
	default:
		f.t.Errorf("fakeAPI: 未模拟的接口 %s", action)
		f.fail(w, "InvalidAction", "not mocked: "+action)
	}
}

func dryKey(dry bool) string {
	if dry {
		return "CreateInstances_dry"
	}
	return "CreateInstances_real"
}

// body 取建机预检（CreateInstances_dry）或真建（CreateInstances_real）最近一次的请求体。
func (f *fakeAPI) body(key string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[key]
}

func (f *fakeAPI) each(body map[string]any, fn func(*fakeInstance)) {
	ids, _ := body["InstanceIds"].([]any)
	if instID, ok := body["InstanceId"].(string); ok { // 单实例接口（如 ResetInstance）
		ids = []any{instID}
	}
	for _, raw := range ids {
		if inst := f.instances[raw.(string)]; inst != nil {
			fn(inst)
		}
	}
}

func (f *fakeAPI) write(w http.ResponseWriter, resp map[string]any) {
	resp["RequestId"] = "req-fake"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"Response": resp})
}

func (f *fakeAPI) fail(w http.ResponseWriter, code, msg string) {
	f.write(w, map[string]any{"Error": map[string]any{"Code": code, "Message": msg}})
}

func (i *fakeInstance) json() map[string]any {
	public := []string{}
	if i.publicIP != "" {
		public = append(public, i.publicIP)
	}
	tags := []map[string]any{}
	for _, t := range i.tags {
		tags = append(tags, map[string]any{"Key": t.Key, "Value": t.Value})
	}
	return map[string]any{
		"InstanceId": i.id, "InstanceName": i.name, "InstanceState": i.state, "BundleId": i.bundle,
		"CPU": 2, "Memory": 2, "SystemDisk": map[string]any{"DiskSize": 40},
		"PublicAddresses": public, "PrivateAddresses": []string{"10.0.0.5"},
		"Platform": i.platform, "PlatformType": "LINUX_UNIX", "OsName": i.osName,
		"Zone": i.zone, "Tags": tags, "CreatedTime": "2026-11-24T08:00:00Z", "ExpiredTime": i.expired,
		"RenewFlag":       i.renew,
		"LatestOperation": i.latestOp, "LatestOperationState": i.opState, "LatestOperationErrorMsg": i.opErr,
	}
}

// lastIDs 取某动作最近一次请求里的 InstanceIds。
func (f *fakeAPI) lastIDs(action string) []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastIDsOf[action]
}
