package cvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeAPI 模拟腾讯云 API 3.0 的 CVM / VPC 接口：按 X-TC-Action 与 X-TC-Region
// 分发，维护一张测试可改动的实例表。异步接口不立刻改状态，而是登记一个过渡，
// 之后第 N 次查询该实例时才落到终态，用来驱动 ActionStatus 与重装的等待链路。
type fakeAPI struct {
	t *testing.T

	mu             sync.Mutex
	regions        []string
	instances      map[string]*fakeInstance // 实例 ID → 实例
	vpcs           map[string][]vpc         // 地域 → VPC
	subnets        map[string][]subnet      // 地域 → 子网
	images         map[string][]image       // 地域 → 镜像
	keyPairs       map[string][]keyPair     // 地域 → 密钥对
	types          map[string][]instanceTypeConfig
	failList       map[string]bool // 这些地域的 DescribeInstances 报错
	diskOK         map[string]bool // RunInstances 预检接受的云盘类型；不在表里的一律报不支持
	resetPolls     int             // 重装从下发到结束要经过的查询次数
	resetFail      bool            // 重装最终以失败收场
	resetReject    bool            // 重装请求被平台直接拒绝（如镜像不适配机型）
	busyStarts     int             // 接下来这么多次开机请求会被"操作进行中"拒绝
	resetAutoStart bool            // 重装完成后平台自动把实例开起来
	alwaysRunning  bool            // 开机请求一律被平台以"已在运行"拒绝
	priceFail      bool            // 询价接口报错（模拟实例还在创建中、平台给不出价）
	nextID         int
	calls          []fakeCall // 按顺序记录每次调用，断言用
}

type fakeCall struct {
	region, action string
	body           map[string]any
}

type fakeInstance struct {
	id, region, zone, name, instanceType, state, charge, imageID, publicIP string
	tags                                                                   []tag
	latestOp, opState, opErr                                               string

	// pending 是已下发未完成的异步操作：还剩几次查询后落到 final
	pending *pendingOp
}

type pendingOp struct {
	pollsLeft int
	final     func(*fakeInstance)
}

func newFakeAPI(t *testing.T) *fakeAPI {
	return &fakeAPI{
		t:          t,
		instances:  map[string]*fakeInstance{},
		vpcs:       map[string][]vpc{},
		subnets:    map[string][]subnet{},
		images:     map[string][]image{},
		keyPairs:   map[string][]keyPair{},
		types:      map[string][]instanceTypeConfig{},
		failList:   map[string]bool{},
		diskOK:     map[string]bool{"CLOUD_BSSD": true},
		resetPolls: 1,
	}
}

func (f *fakeAPI) add(inst fakeInstance) *fakeInstance {
	if inst.charge == "" {
		inst.charge = postpaid // 缺省按量；要造包年包月实例就显式给 charge
	}
	f.instances[inst.id] = &inst
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

// lastBody 返回最近一次该动作的请求体。
func (f *fakeAPI) lastBody(action string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].action == action {
			return f.calls[i].body
		}
	}
	return nil
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
	action, region := r.Header.Get("X-TC-Action"), r.Header.Get("X-TC-Region")
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{region, action, body})
	if r.Header.Get("Authorization") == "" || r.Host != host && r.Host != vpcHost {
		f.t.Errorf("请求 %s 缺 Authorization 或 Host 不对: %q", action, r.Host)
	}

	switch action {
	case "DescribeRegions":
		set := make([]map[string]any, 0, len(f.regions))
		for _, reg := range f.regions {
			set = append(set, map[string]any{"Region": reg, "RegionName": reg + "区"})
		}
		f.write(w, map[string]any{"RegionSet": set})
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
	case "StopInstances":
		f.each(body, func(inst *fakeInstance) {
			inst.latestOp, inst.opState, inst.opErr = "StopInstances", "OPERATE_RUNNING", ""
			inst.state = "STOPPING"
			inst.pending = &pendingOp{2, func(i *fakeInstance) { i.state, i.opState = "STOPPED", "OPERATE_SUCCESS" }}
		})
		f.write(w, map[string]any{})
	case "StartInstances":
		if f.alwaysRunning {
			f.fail(w, "UnsupportedOperation.InstanceStateRunning", "The request does not support the instance which is in the state of `RUNNING`.")
			return
		}
		if f.busyStarts > 0 {
			f.busyStarts--
			f.fail(w, "OperationDenied.InstanceOperationInProgress", "The instance already has operation in progress. Please try again later.")
			return
		}
		f.each(body, func(inst *fakeInstance) {
			inst.latestOp, inst.opState, inst.opErr = "StartInstances", "OPERATE_RUNNING", ""
			inst.state = "STARTING"
			inst.pending = &pendingOp{2, func(i *fakeInstance) { i.state, i.opState = "RUNNING", "OPERATE_SUCCESS" }}
		})
		f.write(w, map[string]any{})
	case "TerminateInstances":
		ids, _ := body["InstanceIds"].([]any)
		if inst := f.instances[ids[0].(string)]; inst == nil {
			f.fail(w, "InvalidInstanceId.NotFound", "The specified instance ID is not found.")
			return
		}
		f.each(body, func(inst *fakeInstance) { delete(f.instances, inst.id) })
		f.write(w, map[string]any{})
	case "ResetInstance":
		if f.resetReject {
			f.fail(w, "InvalidParameterValue.InvalidImageForGivenInstanceType", "The specified image is not available for the given instance type.")
			return
		}
		f.each(body, func(inst *fakeInstance) {
			inst.latestOp, inst.opState, inst.opErr = "ResetInstance", "OPERATE_RUNNING", ""
			inst.pending = &pendingOp{f.resetPolls, func(i *fakeInstance) {
				if f.resetFail {
					i.opState, i.opErr = "OPERATE_FAILED", "image not available"
					return
				}
				i.opState, i.imageID = "OPERATE_SUCCESS", body["ImageId"].(string)
				if f.resetAutoStart {
					i.state = "RUNNING"
				}
			}}
		})
		f.write(w, map[string]any{})
	case "ResetInstancesType":
		f.each(body, func(inst *fakeInstance) {
			inst.latestOp, inst.opState = "ResetInstancesType", "OPERATE_RUNNING"
			inst.pending = &pendingOp{2, func(i *fakeInstance) {
				i.instanceType, i.opState = body["InstanceType"].(string), "OPERATE_SUCCESS"
			}}
		})
		f.write(w, map[string]any{})
	case "RunInstances":
		if dry, _ := body["DryRun"].(bool); dry {
			disk, _ := body["SystemDisk"].(map[string]any)["DiskType"].(string)
			if !f.diskOK[disk] {
				f.fail(w, "InvalidParameter", "[19045]CVM not support the required disk")
				return
			}
			f.write(w, map[string]any{})
			return
		}
		f.nextID++
		id := fmt.Sprintf("ins-test%04d", f.nextID)
		placement, _ := body["Placement"].(map[string]any)
		f.instances[id] = &fakeInstance{
			id: id, region: region, zone: placement["Zone"].(string), name: body["InstanceName"].(string),
			instanceType: body["InstanceType"].(string), state: "PENDING", charge: postpaid,
			imageID: body["ImageId"].(string), publicIP: fmt.Sprintf("203.0.113.%d", f.nextID),
		}
		f.write(w, map[string]any{"InstanceIdSet": []string{id}})
	case "DescribeImages":
		var want []string
		if name, _ := filterName(body); name == "image-id" {
			want = filterValues(body)
		}
		set := []map[string]any{}
		for _, im := range f.images[region] {
			if len(want) > 0 && !containsString(want, im.ImageId) {
				continue
			}
			set = append(set, map[string]any{
				"ImageId": im.ImageId, "ImageName": im.ImageName, "Platform": im.Platform, "OsType": im.OsType,
				"Architecture": im.Architecture, "ImageState": im.ImageState, "ImageType": im.ImageType,
			})
		}
		f.write(w, map[string]any{"TotalCount": len(set), "ImageSet": set})
	case "DescribeInstanceTypeConfigs":
		set := []map[string]any{}
		for _, tc := range f.types[region] {
			set = append(set, map[string]any{
				"InstanceType": tc.InstanceType, "InstanceFamily": tc.InstanceFamily, "CPU": tc.CPU,
				"Memory": tc.Memory, "GPU": tc.GPU, "FPGA": tc.FPGA,
			})
		}
		f.write(w, map[string]any{"InstanceTypeConfigSet": set})
	case "DescribeKeyPairs":
		set := []map[string]any{}
		for _, k := range f.keyPairs[region] {
			set = append(set, map[string]any{"KeyPairId": k.KeyPairId, "KeyName": k.KeyName, "PublicKey": k.PublicKey})
		}
		f.write(w, map[string]any{"KeyPairSet": set})
	case "DescribeVpcs":
		set := []map[string]any{}
		for _, v := range f.vpcs[region] {
			set = append(set, map[string]any{"VpcId": v.VpcId, "IsDefault": v.IsDefault})
		}
		f.write(w, map[string]any{"VpcSet": set})
	case "DescribeSubnets":
		set := []map[string]any{}
		for _, s := range f.subnets[region] {
			set = append(set, map[string]any{
				"SubnetId": s.SubnetId, "VpcId": s.VpcId, "Zone": s.Zone, "IsDefault": s.IsDefault,
				"AvailableIpAddressCount": s.AvailableIpAddressCount,
			})
		}
		f.write(w, map[string]any{"SubnetSet": set})
	case "InquiryPriceRunInstances":
		if f.priceFail {
			f.fail(w, "InternalError", "price not ready")
			return
		}
		f.write(w, map[string]any{"Price": map[string]any{"InstancePrice": map[string]any{"UnitPrice": 0.26, "UnitPriceDiscount": 0.2}}})
	default:
		f.t.Errorf("fakeAPI: 未模拟的接口 %s", action)
		f.fail(w, "InvalidAction", "not mocked: "+action)
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

// each 对请求体 InstanceIds 里的每个实例执行 fn；不存在的实例 ID 报 NotFound。
func (f *fakeAPI) each(body map[string]any, fn func(*fakeInstance)) {
	ids, _ := body["InstanceIds"].([]any)
	if instID, ok := body["InstanceId"].(string); ok {
		ids = []any{instID}
	}
	for _, raw := range ids {
		if inst := f.instances[raw.(string)]; inst != nil {
			fn(inst)
		}
	}
}

// filterName 取 Filters[0] 的 Name。
func filterName(body map[string]any) (string, bool) {
	filters, _ := body["Filters"].([]any)
	if len(filters) == 0 {
		return "", false
	}
	name, _ := filters[0].(map[string]any)["Name"].(string)
	return name, true
}

// filterValues 取 Filters[0] 的 Values。
func filterValues(body map[string]any) []string {
	filters, _ := body["Filters"].([]any)
	if len(filters) == 0 {
		return nil
	}
	vals, _ := filters[0].(map[string]any)["Values"].([]any)
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		out = append(out, v.(string))
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func (i *fakeInstance) json() map[string]any {
	tags := make([]map[string]any, 0, len(i.tags))
	for _, t := range i.tags {
		tags = append(tags, map[string]any{"Key": t.Key, "Value": t.Value})
	}
	public := []string{}
	if i.publicIP != "" {
		public = []string{i.publicIP}
	}
	return map[string]any{
		"InstanceId": i.id, "InstanceName": i.name, "InstanceType": i.instanceType, "InstanceState": i.state,
		"InstanceChargeType": i.charge, "CPU": 2, "Memory": 2, "CreatedTime": "2026-11-24T08:00:00Z",
		"PublicIpAddresses": public, "PrivateIpAddresses": []string{"10.0.0.5"}, "ImageId": i.imageID,
		"Placement":       map[string]any{"Zone": i.zone},
		"SystemDisk":      map[string]any{"DiskId": "disk-" + i.id, "DiskType": "CLOUD_BSSD", "DiskSize": 50},
		"Tags":            tags,
		"LatestOperation": i.latestOp, "LatestOperationState": i.opState, "LatestOperationErrorMsg": i.opErr,
	}
}
