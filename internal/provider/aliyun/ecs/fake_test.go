package ecs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeAPI 模拟阿里云 ECS OpenAPI：按 x-acs-action 与 RegionId 分发，维护一张测试
// 可改动的实例表。异步接口不立刻改状态，而是登记一个过渡，之后第 N 次查询该
// 实例时才落到终态，用来驱动 ActionStatus 与重装的等待链路。
type fakeAPI struct {
	t *testing.T

	mu            sync.Mutex
	regions       []regionInfo
	zones         map[string][]string        // 地域 → 可用区
	instances     map[string]*fakeInstance   // 实例 ID → 实例
	switches      map[string][]vswitch       // 地域 → 交换机
	groups        map[string][]securityGroup // 地域 → 安全组
	images        map[string][]image         // 地域 → 镜像
	keyPairs      map[string][]keyPair       // 地域 → 密钥对
	types         map[string][]instanceType  // 地域 → 全部规格
	available     map[string][]string        // 地域 → 在售规格 ID
	diskOK        map[string]bool            // RunInstances 预检接受的云盘类型
	failList      map[string]bool            // 这些地域的 DescribeInstances 报错
	renewPrices   map[string]float64         // 包年包月实例 ID → 续一个月的价格
	failRenew     bool                       // DescribeRenewalPrice 报错
	failAutoRenew bool                       // DescribeInstanceAutoRenewAttribute 报错
	replacePolls  int                        // 换系统盘从下发到结束要经过的查询次数
	replaceReject bool                       // 换系统盘请求被平台直接拒绝
	nextID        int
	calls         []fakeCall
}

type fakeCall struct {
	host, action, region string
	params               map[string]string
}

// fakeInstance 的导出字段名与接口字段一致，可直接 JSON 编码进响应。
type fakeInstance struct {
	InstanceId         string
	InstanceName       string
	InstanceType       string
	Status             string
	ZoneId             string
	RegionId           string
	InstanceChargeType string
	Cpu                int
	Memory             int
	CreationTime       string
	ExpiredTime        string // 包年包月实例的到期时间；按量实例为空
	ImageId            string
	PublicIpAddress    struct {
		IpAddress []string `json:"IpAddress"`
	} `json:"PublicIpAddress"`
	EipAddress struct {
		IpAddress string `json:"IpAddress"`
	} `json:"EipAddress"`
	VpcAttributes struct {
		PrivateIpAddress struct {
			IpAddress []string `json:"IpAddress"`
		} `json:"PrivateIpAddress"`
	} `json:"VpcAttributes"`
	OperationLocks struct {
		LockReason []struct {
			LockReason string `json:"LockReason"`
		} `json:"LockReason"`
	} `json:"OperationLocks"`

	tags    map[string]string // 标签键 → 值（ListTagResources 用）
	sysDisk disk
	locks   int // 未完成的操作锁个数
	// autoRenew 是包年包月实例有没有开自动续费（DescribeInstanceAutoRenewAttribute 用；
	// 不进 DescribeInstances 的响应）
	autoRenew bool

	// pending 是已下发未完成的异步操作：还剩几次查询后落到 final
	pending *pendingOp
}

type pendingOp struct {
	pollsLeft int
	final     func(*fakeInstance)
}

func newFakeAPI(t *testing.T) *fakeAPI {
	return &fakeAPI{
		t: t, instances: map[string]*fakeInstance{}, zones: map[string][]string{},
		switches: map[string][]vswitch{}, groups: map[string][]securityGroup{}, images: map[string][]image{},
		keyPairs: map[string][]keyPair{}, types: map[string][]instanceType{}, available: map[string][]string{},
		diskOK: map[string]bool{"cloud_essd": true}, failList: map[string]bool{}, replacePolls: 1,
		renewPrices: map[string]float64{},
	}
}

func (f *fakeAPI) add(inst fakeInstance) *fakeInstance {
	if inst.InstanceChargeType == "" {
		inst.InstanceChargeType = postpaid // 缺省按量；要造包年包月实例就显式给 PrePaid
	}
	inst.sysDisk = disk{DiskId: "disk-" + inst.InstanceId, Type: "system", Category: "cloud_essd", Size: 40}
	f.instances[inst.InstanceId] = &inst
	return &inst
}

// actions 返回调用过的"地域 动作"序列。
func (f *fakeAPI) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.region+" "+c.action)
	}
	return out
}

// lastParams 返回最近一次该动作的参数。
func (f *fakeAPI) lastParams(action string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].action == action {
			return f.calls[i].params
		}
	}
	return nil
}

// paramsOf 按调用顺序返回该动作每次调用的参数。
func (f *fakeAPI) paramsOf(action string) []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]string
	for _, c := range f.calls {
		if c.action == action {
			out = append(out, c.params)
		}
	}
	return out
}

// reset 清空调用记录，便于只统计某一段的调用。
func (f *fakeAPI) reset() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

func (f *fakeAPI) get(id string) *fakeInstance {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.instances[id]
}

// client 起一个 httptest 服务并让被测客户端打到它；轮询间隔压到最小、等待不真睡。
func (f *fakeAPI) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New("acct-1", "AKIDtest", "test-secret", srv.Client())
	c.api.SetEndpoint(srv.URL)
	c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	c.pollEvery = time.Millisecond
	return c
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	action := r.Header.Get("X-Acs-Action")
	params := map[string]string{}
	for k := range r.URL.Query() {
		params[k] = r.URL.Query().Get(k)
	}
	region := params["RegionId"]

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{r.Host, action, region, params})
	// 每个请求都必须打对应的接入点并带签名：地域接口走地域专属域名，全局接口走中心域名
	wantHost := ecsHost(region)
	if action == "DescribeRegions" {
		wantHost = centralHost
	}
	if r.Header.Get("Authorization") == "" || r.Host != wantHost {
		f.t.Errorf("请求 %s 缺 Authorization 或接入点不对: got %q, want %q", action, r.Host, wantHost)
	}

	switch action {
	case "DescribeRegions":
		set := make([]map[string]any, 0, len(f.regions))
		for _, reg := range f.regions {
			set = append(set, map[string]any{"RegionId": reg.ID, "LocalName": reg.Name})
		}
		f.ok(w, map[string]any{"Regions": map[string]any{"Region": set}})
	case "DescribeZones":
		set := []map[string]any{}
		for _, z := range f.zones[region] {
			set = append(set, map[string]any{"ZoneId": z})
		}
		f.ok(w, map[string]any{"Zones": map[string]any{"Zone": set}})
	case "DescribeInstances":
		if f.failList[region] {
			f.fail(w, "InternalError", "boom")
			return
		}
		set := []json.RawMessage{}
		for _, inst := range f.instances {
			if inst.RegionId != region {
				continue
			}
			if inst.pending != nil {
				inst.pending.pollsLeft--
				if inst.pending.pollsLeft <= 0 {
					inst.pending.final(inst)
					inst.pending = nil
				}
			}
			inst.OperationLocks.LockReason = inst.OperationLocks.LockReason[:0]
			for i := 0; i < inst.locks; i++ {
				inst.OperationLocks.LockReason = append(inst.OperationLocks.LockReason, struct {
					LockReason string `json:"LockReason"`
				}{"Locking"})
			}
			b, _ := json.Marshal(inst)
			set = append(set, b)
		}
		f.ok(w, map[string]any{"TotalCount": len(set), "Instances": map[string]any{"Instance": set}})
	case "StopInstance":
		f.each(params, func(inst *fakeInstance) {
			inst.Status = "Stopping"
			inst.pending = &pendingOp{2, func(i *fakeInstance) { i.Status = "Stopped" }}
		})
		f.ok(w, map[string]any{})
	case "StartInstance":
		f.each(params, func(inst *fakeInstance) {
			inst.Status = "Starting"
			inst.pending = &pendingOp{2, func(i *fakeInstance) { i.Status = "Running" }}
		})
		f.ok(w, map[string]any{})
	case "DeleteInstance":
		inst := f.instances[params["InstanceId"]]
		if inst == nil {
			f.fail(w, "InvalidInstanceId.NotFound", "The specified InstanceId does not exist.")
			return
		}
		delete(f.instances, inst.InstanceId)
		f.ok(w, map[string]any{})
	case "ReplaceSystemDisk":
		if f.replaceReject {
			f.fail(w, "InvalidOperation.TargetImageIncompatible", "The specified image cannot be used to replace the system disk.")
			return
		}
		f.each(params, func(inst *fakeInstance) {
			inst.locks = 1
			inst.pending = &pendingOp{f.replacePolls, func(i *fakeInstance) {
				i.locks = 0
				i.ImageId = params["ImageId"]
				i.sysDisk = disk{DiskId: "disk-new-" + i.InstanceId, Type: "system", Category: "cloud_essd", Size: 40}
			}}
		})
		f.ok(w, map[string]any{"DiskId": "disk-new"})
	case "ModifyInstanceSpec":
		f.each(params, func(inst *fakeInstance) { inst.InstanceType = params["InstanceType"] })
		f.ok(w, map[string]any{})
	case "RunInstances":
		if params["DryRun"] == "true" {
			category := params["SystemDisk.Category"]
			if !f.diskOK[category] {
				f.fail(w, "InvalidInstanceType.NotSupportDiskCategory", "The specified InstanceType does not support the specified disk category.")
				return
			}
			// 预检通过时阿里云回的就是这个"错误码"
			f.fail(w, "DryRunOperation", "Request validation has been passed with DryRun flag set.")
			return
		}
		f.nextID++
		id := fmt.Sprintf("i-test%04d", f.nextID)
		inst := &fakeInstance{
			InstanceId: id, InstanceName: params["InstanceName"], InstanceType: params["InstanceType"],
			Status: "Pending", InstanceChargeType: postpaid, ImageId: params["ImageId"],
			ZoneId: params["ZoneId"], RegionId: region, Cpu: 2, Memory: 2048, CreationTime: "2026-11-24T08:00Z",
			sysDisk: disk{DiskId: "disk-" + id, Type: "system", Category: params["SystemDisk.Category"], Size: 40},
		}
		inst.PublicIpAddress.IpAddress = []string{fmt.Sprintf("203.0.113.%d", f.nextID)}
		f.instances[id] = inst
		f.ok(w, map[string]any{"InstanceIdSets": map[string]any{"InstanceIdSet": []string{id}}})
	case "DescribeImages":
		want := params["ImageId"]
		set := []map[string]any{}
		for _, im := range f.images[region] {
			if want != "" && im.ImageId != want {
				continue
			}
			if params["OSType"] == "linux" && im.Platform == "Windows Server" {
				continue // 模拟服务端按操作系统类型过滤
			}
			set = append(set, map[string]any{
				"ImageId": im.ImageId, "ImageName": im.ImageName, "OSNameEn": im.OSNameEn, "Platform": im.Platform,
				"Architecture": im.Architecture, "Status": im.Status, "Usage": im.Usage, "CreationTime": im.CreationTime,
			})
		}
		f.ok(w, map[string]any{"TotalCount": len(set), "Images": map[string]any{"Image": set}})
	case "DescribeInstanceTypes":
		set := []map[string]any{}
		for _, t := range f.types[region] {
			set = append(set, map[string]any{
				"InstanceTypeId": t.InstanceTypeId, "InstanceTypeFamily": t.InstanceTypeFamily, "CpuCoreCount": t.CpuCoreCount,
				"MemorySize": t.MemorySize, "GPUAmount": t.GPUAmount,
			})
		}
		f.ok(w, map[string]any{"InstanceTypes": map[string]any{"InstanceType": set}})
	case "DescribeAvailableResource":
		res := []map[string]any{}
		for _, id := range f.available[region] {
			res = append(res, map[string]any{"Status": "Available", "Value": id})
		}
		f.ok(w, map[string]any{"AvailableZones": map[string]any{"AvailableZone": []any{map[string]any{
			"AvailableResources": map[string]any{"AvailableResource": []any{map[string]any{
				"SupportedResources": map[string]any{"SupportedResource": res},
			}}},
		}}}})
	case "DescribeKeyPairs":
		set := []map[string]any{}
		for _, k := range f.keyPairs[region] {
			set = append(set, map[string]any{"KeyPairName": k.KeyPairName, "KeyPairFingerPrint": k.KeyPairFingerPrint})
		}
		f.ok(w, map[string]any{"KeyPairs": map[string]any{"KeyPair": set}})
	case "DescribeVSwitches":
		set := []map[string]any{}
		for _, v := range f.switches[region] {
			if zone := params["ZoneId"]; zone != "" && v.ZoneId != zone {
				continue
			}
			set = append(set, map[string]any{
				"VSwitchId": v.VSwitchId, "VpcId": v.VpcId, "ZoneId": v.ZoneId, "IsDefault": v.IsDefault,
				"AvailableIpAddressCount": v.AvailableIpAddressCount,
			})
		}
		f.ok(w, map[string]any{"VSwitches": map[string]any{"VSwitch": set}})
	case "DescribeSecurityGroups":
		set := []map[string]any{}
		for _, g := range f.groups[region] {
			if vpc := params["VpcId"]; vpc != "" && g.VpcId != vpc {
				continue
			}
			set = append(set, map[string]any{"SecurityGroupId": g.SecurityGroupId, "VpcId": g.VpcId})
		}
		f.ok(w, map[string]any{"SecurityGroups": map[string]any{"SecurityGroup": set}})
	case "DescribeDisks":
		inst := f.instances[params["InstanceId"]]
		if inst == nil {
			f.fail(w, "InvalidInstanceId.NotFound", "not found")
			return
		}
		d := inst.sysDisk
		f.ok(w, map[string]any{"Disks": map[string]any{"Disk": []any{map[string]any{
			"DiskId": d.DiskId, "Type": d.Type, "Category": d.Category, "Size": d.Size,
		}}}})
	case "ListTagResources":
		list := []map[string]any{}
		for i := 1; ; i++ {
			id := params[fmt.Sprintf("ResourceId.%d", i)]
			if id == "" {
				break
			}
			if inst := f.instances[id]; inst != nil {
				keys := make([]string, 0, len(inst.tags))
				for k := range inst.tags {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					list = append(list, map[string]any{"ResourceId": id, "TagKey": k, "TagValue": inst.tags[k]})
				}
			}
		}
		f.ok(w, map[string]any{"TagResources": map[string]any{"TagResource": list}})
	case "DescribePrice":
		f.ok(w, map[string]any{"PriceInfo": map[string]any{"Price": map[string]any{"TradePrice": 0.42, "Currency": "CNY"}}})
	case "DescribeRenewalPrice":
		// 只给必填参数 = 续费 1 个月的价格；只认包年包月实例
		inst := f.instances[params["ResourceId"]]
		if f.failRenew || inst == nil || inst.InstanceChargeType != prepaid {
			f.fail(w, "InvalidResourceId.NotFound", "not found or not prepaid")
			return
		}
		price, ok := f.renewPrices[inst.InstanceId]
		if !ok {
			price = 100
		}
		f.ok(w, map[string]any{"PriceInfo": map[string]any{"Price": map[string]any{
			"OriginalPrice": price, "DiscountPrice": 0, "Currency": "CNY", "TradePrice": price,
		}}})
	case "DescribeInstanceAutoRenewAttribute":
		// 照平台的真实行为：InstanceId 与 RenewalStatus 至少给一个，都不给就拒绝
		// （接口元数据把 InstanceId 标成非必填，实际不是——2026-11 在真实账号上踩到）
		if f.failAutoRenew {
			f.fail(w, "InternalError", "boom")
			return
		}
		if params["InstanceId"] == "" && params["RenewalStatus"] == "" {
			f.fail(w, "InvalidParameter.RenewalStatusInstanceId",
				"The parameter RenewalStatus and InstanceId can not be both empty.")
			return
		}
		set := []map[string]any{}
		if inst := f.instances[params["InstanceId"]]; inst != nil && inst.InstanceChargeType == prepaid {
			status := "Normal"
			if inst.autoRenew {
				status = "AutoRenewal"
			}
			set = append(set, map[string]any{"InstanceId": inst.InstanceId, "AutoRenewEnabled": inst.autoRenew,
				"RenewalStatus": status, "Duration": 1, "PeriodUnit": "Month"})
		}
		f.ok(w, map[string]any{"PageNumber": 1, "PageSize": 100, "TotalCount": len(set),
			"InstanceRenewAttributes": map[string]any{"InstanceRenewAttribute": set}})
	default:
		f.t.Errorf("fakeAPI: 未模拟的接口 %s", action)
		f.fail(w, "InvalidAction.NotFound", "not mocked: "+action)
	}
}

func (f *fakeAPI) ok(w http.ResponseWriter, body map[string]any) {
	body["RequestId"] = "req-fake"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeAPI) fail(w http.ResponseWriter, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": "req-fake", "Code": code, "Message": msg})
}

// each 对请求体 InstanceId 指向的实例执行 fn；不存在时什么都不做。
func (f *fakeAPI) each(params map[string]string, fn func(*fakeInstance)) {
	if inst := f.instances[params["InstanceId"]]; inst != nil {
		fn(inst)
	}
}
