// Package lighthouse 实现 provider.Provider 的腾讯云轻量应用服务器（包年包月）适配。
//
// 和云服务器（CVM）是一条产品线里的两种机器，接口与语义都不同：
//   - **建机即付一个月**：Period=1、自动续费（NOTIFY_AND_AUTO_RENEW）。每次建机都先
//     发一遍 DryRun 预检，参数 / 库存 / 余额的问题在扣费前报出来。
//   - **没有 cloud-init 用户数据通道**：随机 root 密码经平台的"登录密码"下发，落在镜像的
//     默认用户上（Ubuntu 是 ubuntu），root 默认禁止 SSH 登录。所以建机时顺带下发一条
//     "创建后自动执行的命令"（InitCommand），等默认用户的密码落盘后把密码哈希原样复制给
//     root 并打开 root 密码登录——命令里不出现明文密码。高位口 / 443 隧道 / 运维公钥 /
//     自定义 user-data 这些靠 cloud-init 的置备一律明确拒绝。
//   - 异步接口没有任务号：靠实例上的 LatestOperation / LatestOperationState 判定进度。
//   - 套餐（Bundle）自带月价，按量那一套逐台询价在这里用不上。
//
// 文档: https://cloud.tencent.com/document/api/1207/47576 （数据结构）
package lighthouse

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
	"github.com/hoshinojian/vpsctl/internal/provider/tencent"
)

// ProviderName 是 accounts.json 里 provider 字段的取值。
const ProviderName = "tencent-lighthouse"

const (
	host            = "lighthouse.tencentcloudapi.com"
	service         = "lighthouse"
	version         = "2020-03-24"
	currency        = "CNY"
	pageSize        = 100
	maxPages        = 1000 // 分页安全上限，防 TotalCount 异常时死循环
	listConcurrency = 12
	catalogTTL      = time.Hour
	// 建机固定买一个月、到期自动续费；要停就在控制台销毁，或改续费标志。
	prepaidPeriod = 1
	autoRenewFlag = "NOTIFY_AND_AUTO_RENEW"
)

// errNotFound 表示实例在该账号下不存在（所有地域都找不到）。
var errNotFound = errors.New("实例不存在")

// ---- 腾讯云 API 3.0 结构（仅本工具用到的字段） ----

type tag struct {
	Key   string `json:"Key"`
	Value string `json:"Value"`
}

// instance 是 DescribeInstances 的 InstanceSet 元素。
type instance struct {
	InstanceId         string `json:"InstanceId"`
	InstanceName       string `json:"InstanceName"`
	InstanceState      string `json:"InstanceState"`
	InstanceChargeType string `json:"InstanceChargeType"`
	RenewFlag          string `json:"RenewFlag"`
	BundleId           string `json:"BundleId"`
	BlueprintId        string `json:"BlueprintId"`
	CPU                int    `json:"CPU"`
	Memory             int    `json:"Memory"` // GB
	SystemDisk         struct {
		DiskSize int `json:"DiskSize"`
	} `json:"SystemDisk"`
	PublicAddresses  []string `json:"PublicAddresses"`
	PrivateAddresses []string `json:"PrivateAddresses"`
	Platform         string   `json:"Platform"`
	PlatformType     string   `json:"PlatformType"`
	OsName           string   `json:"OsName"`
	Zone             string   `json:"Zone"`
	Tags             []tag    `json:"Tags"`
	CreatedTime      string   `json:"CreatedTime"`
	ExpiredTime      string   `json:"ExpiredTime"`

	// 最新一次异步操作及其状态（SUCCESS / OPERATING / FAILED），可能是上一轮留下的旧值
	LatestOperation         string `json:"LatestOperation"`
	LatestOperationState    string `json:"LatestOperationState"`
	LatestOperationErrorMsg string `json:"LatestOperationErrorMsg"`
}

type instancesPage struct {
	TotalCount  int        `json:"TotalCount"`
	InstanceSet []instance `json:"InstanceSet"`
}

type bundle struct {
	BundleId                 string `json:"BundleId"`
	CPU                      int    `json:"CPU"`
	Memory                   int    `json:"Memory"` // GB
	SystemDiskSize           int    `json:"SystemDiskSize"`
	BundleSalesState         string `json:"BundleSalesState"`
	SupportLinuxUnixPlatform bool   `json:"SupportLinuxUnixPlatform"`
	Price                    struct {
		InstancePrice struct {
			Currency      string  `json:"Currency"`
			DiscountPrice float64 `json:"DiscountPrice"`
			OriginalPrice float64 `json:"OriginalPrice"`
		} `json:"InstancePrice"`
	} `json:"Price"`
}

type blueprint struct {
	BlueprintId    string `json:"BlueprintId"`
	BlueprintName  string `json:"BlueprintName"`
	OsName         string `json:"OsName"`
	Platform       string `json:"Platform"`
	PlatformType   string `json:"PlatformType"`
	BlueprintType  string `json:"BlueprintType"` // PURE_OS 才是纯系统镜像；APP_OS / DOCKER 是预装应用的
	BlueprintState string `json:"BlueprintState"`
}

type keyPair struct {
	KeyPairId string `json:"KeyPairId"`
	KeyName   string `json:"KeyName"`
	PublicKey string `json:"PublicKey"`
}

type regionInfo struct{ ID, Name string }

// Client 是单账号的腾讯云轻量应用服务器适配，并发安全。
type Client struct {
	account string
	api     *tencent.Client

	// 地域表进程内只取一次（地域基本不变），sync.Once 保证并发下也只打一次接口。
	regionsOnce sync.Once
	regions     []regionInfo
	regionsErr  error

	// index 记录实例 ID → 地域（List / Create 时登记），按 ID 操作时先查它。
	index provider.RegionIndex

	blueprints *provider.TTLCache[[]blueprint]       // region → 全部镜像
	bundles    *provider.TTLCache[map[string]bundle] // region → BundleId → 套餐（带月价）

	// sleep / pollEvery / opWait 供异步链路等待与测试注入
	sleep     func(ctx context.Context, d time.Duration) error
	pollEvery time.Duration
	opWait    time.Duration
}

// New 创建客户端；hc 为 nil 时用带超时的默认客户端。
func New(account, secretID, secretKey string, hc *http.Client) *Client {
	return &Client{
		account:    account,
		api:        tencent.New(secretID, secretKey, hc),
		blueprints: provider.NewTTLCache[[]blueprint](catalogTTL),
		bundles:    provider.NewTTLCache[map[string]bundle](catalogTTL),
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
		pollEvery: 3 * time.Second,
		opWait:    5 * time.Minute,
	}
}

func init() {
	provider.Register(ProviderName, provider.AuthKeyPair, func(account string, cred provider.Credentials, hc *http.Client) (provider.Provider, error) {
		if cred.AccessKey == "" || cred.SecretKey == "" {
			return nil, errors.New("tencent-lighthouse: 缺少 access_key（SecretId）或 secret_key（SecretKey）")
		}
		return New(account, cred.AccessKey, cred.SecretKey, hc), nil
	})
}

// ---- 地域 ----

func (c *Client) regionList(ctx context.Context) ([]regionInfo, error) {
	c.regionsOnce.Do(func() {
		// 首次调用可能带的是短生命周期的 ctx；地域表是一次性数据，用独立 ctx 取，
		// 免得某个调用方取消后缓存下永久失败。
		ctx := context.WithoutCancel(ctx)
		var out struct {
			RegionSet []struct {
				Region     string `json:"Region"`
				RegionName string `json:"RegionName"`
			} `json:"RegionSet"`
		}
		if err := c.api.Call(ctx, host, service, version, "DescribeRegions", "", nil, &out); err != nil {
			c.regionsErr = fmt.Errorf("查询地域失败: %w", err)
			return
		}
		for _, r := range out.RegionSet {
			c.regions = append(c.regions, regionInfo{ID: r.Region, Name: r.RegionName})
		}
	})
	return c.regions, c.regionsErr
}

// resolvePlacement 把 create 传进来的区域 slug 解析成地域与可用区。slug 可以是地域
// （ap-guangzhou）或该地域下的可用区（ap-guangzhou-4）——可用区到 DescribeZones 里核对，
// 不按形状猜。
func (c *Client) resolvePlacement(ctx context.Context, slug string) (region, zone string, err error) {
	regions, err := c.regionList(ctx)
	if err != nil {
		return "", "", err
	}
	for _, r := range regions {
		if slug == r.ID {
			return r.ID, "", nil
		}
	}
	var candidates []regionInfo
	for _, r := range regions {
		if strings.HasPrefix(slug, r.ID+"-") {
			candidates = append(candidates, r)
		}
	}
	for _, r := range candidates {
		var out struct {
			ZoneInfoSet []struct {
				Zone string `json:"Zone"`
			} `json:"ZoneInfoSet"`
		}
		if err := c.api.Call(ctx, host, service, version, "DescribeZones", r.ID, nil, &out); err != nil {
			return "", "", fmt.Errorf("查询地域 %s 的可用区失败: %w", r.ID, err)
		}
		for _, z := range out.ZoneInfoSet {
			if z.Zone == slug {
				return r.ID, z.Zone, nil
			}
		}
	}
	return "", "", fmt.Errorf("区域 %q 不是腾讯云轻量应用服务器的地域或可用区（可用: %s）", slug, joinIDs(regions))
}

func joinIDs(regions []regionInfo) string {
	ids := make([]string, 0, len(regions))
	for _, r := range regions {
		ids = append(ids, r.ID)
	}
	return strings.Join(ids, ", ")
}

// ---- 查询 ----

// List 逐地域并发拉取全部实例。任一地域失败即整体失败——编排层的破坏性操作要求
// 账号下节点必须全量可见，宁可不动手。
func (c *Client) List(ctx context.Context) ([]provider.Server, error) {
	regions, err := c.regionList(ctx)
	if err != nil {
		return nil, err
	}
	type result struct {
		servers []provider.Server
		err     error
	}
	results := make([]result, len(regions))
	sem := make(chan struct{}, listConcurrency)
	var wg sync.WaitGroup
	for i, region := range regions {
		wg.Add(1)
		go func(i int, region regionInfo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			servers, err := c.listRegion(ctx, region.ID)
			results[i] = result{servers, err}
		}(i, region)
	}
	wg.Wait()
	out := make([]provider.Server, 0)
	for i, r := range results {
		if r.err != nil {
			return nil, fmt.Errorf("地域 %s: %w", regions[i].ID, r.err)
		}
		out = append(out, r.servers...)
	}
	return out, nil
}

// instancesOf 分页取一个地域的全部实例，并登记 ID → 地域索引。
func (c *Client) instancesOf(ctx context.Context, region string) ([]instance, error) {
	var insts []instance
	for offset := 0; offset < maxPages*pageSize; offset += pageSize {
		var page instancesPage
		err := c.api.Call(ctx, host, service, version, "DescribeInstances", region,
			map[string]any{"Limit": pageSize, "Offset": offset}, &page)
		if err != nil {
			return nil, err
		}
		for _, inst := range page.InstanceSet {
			c.index.Put(inst.InstanceId, region)
		}
		insts = append(insts, page.InstanceSet...)
		if len(page.InstanceSet) == 0 || offset+len(page.InstanceSet) >= page.TotalCount {
			return insts, nil
		}
	}
	return nil, fmt.Errorf("地域 %s 的实例超过 %d 页，放弃遍历", region, maxPages)
}

// listRegion 把一个地域的实例映射成统一视图（月价取自各自的套餐）。
func (c *Client) listRegion(ctx context.Context, region string) ([]provider.Server, error) {
	insts, err := c.instancesOf(ctx, region)
	if err != nil || len(insts) == 0 {
		return nil, err
	}
	bundles, err := c.bundlesOf(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("查询地域 %s 的套餐失败: %w", region, err)
	}
	out := make([]provider.Server, len(insts))
	for i, inst := range insts {
		out[i] = c.toServer(region, inst, bundles[inst.BundleId])
	}
	return out, nil
}

// Get 按实例 ID 取详情。地域取自索引，未登记过则逐地域找一次。
func (c *Client) Get(ctx context.Context, id string) (provider.Server, error) {
	region, inst, err := c.lookup(ctx, id)
	if err != nil {
		return provider.Server{}, err
	}
	bundles, err := c.bundlesOf(ctx, region)
	if err != nil {
		return provider.Server{}, fmt.Errorf("查询地域 %s 的套餐失败: %w", region, err)
	}
	return c.toServer(region, inst, bundles[inst.BundleId]), nil
}

// lookup 找到实例并返回其地域与详情。
func (c *Client) lookup(ctx context.Context, id string) (string, instance, error) {
	region, ok := c.index.Get(id)
	if !ok {
		var err error
		if region, err = c.locate(ctx, id); err != nil {
			return "", instance{}, err
		}
	}
	insts, err := c.instancesOf(ctx, region)
	if err != nil {
		return "", instance{}, err
	}
	for _, inst := range insts {
		if inst.InstanceId == id {
			return region, inst, nil
		}
	}
	return "", instance{}, fmt.Errorf("%w: %s", errNotFound, id)
}

// locate 在所有地域里并发找一台实例（索引未命中时的兜底）；找到即登记并
// 取消其余查询。
func (c *Client) locate(ctx context.Context, id string) (string, error) {
	regions, err := c.regionList(ctx)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	found := make(chan string, 1)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	sem := make(chan struct{}, listConcurrency)
	for _, r := range regions {
		wg.Add(1)
		go func(region string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			insts, err := c.instancesOf(ctx, region)
			if err != nil {
				// "这个地域没有这台机器"是正常结果，只有真正的查询失败才记下来
				if ctx.Err() == nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
				return
			}
			for _, inst := range insts {
				if inst.InstanceId == id {
					select {
					case found <- region:
						cancel() // 找到了，叫停其余地域的查询
					default:
					}
					return
				}
			}
		}(r.ID)
	}
	wg.Wait()
	select {
	case region := <-found:
		c.index.Put(id, region)
		return region, nil
	default:
	}
	if firstErr != nil {
		return "", fmt.Errorf("查找实例 %s 所在地域失败: %w", id, firstErr)
	}
	return "", fmt.Errorf("%w: %s", errNotFound, id)
}

// describe 在指定地域按实例 ID 取一台；查不到返回包裹 errNotFound 的错误。
func (c *Client) describe(ctx context.Context, region, id string) (instance, error) {
	insts, err := c.instancesOf(ctx, region)
	if err != nil {
		return instance{}, err
	}
	for _, inst := range insts {
		if inst.InstanceId == id {
			return inst, nil
		}
	}
	return instance{}, fmt.Errorf("%w: %s", errNotFound, id)
}

// status 把实例状态归一到编排层约定的 active / off / new，其余过渡态小写原样
// 透出（stopping / starting / rebooting …），由管理台按"其他"着色。
func status(state string) string {
	switch state {
	case "RUNNING":
		return "active"
	case "STOPPED":
		return "off"
	case "PENDING":
		return "new"
	case "LAUNCH_FAILED":
		return "error"
	default:
		return strings.ToLower(state)
	}
}

// toServer 把实例映射成统一视图；b 是实例所用套餐（月价来自它），查不到时为零值。
func (c *Client) toServer(region string, inst instance, b bundle) provider.Server {
	s := provider.Server{
		ID:          inst.InstanceId,
		Account:     c.account,
		Name:        inst.InstanceName,
		Status:      status(inst.InstanceState),
		Region:      region,
		Size:        inst.BundleId,
		VCPUs:       inst.CPU,
		MemoryMB:    inst.Memory * 1024,
		DiskGB:      inst.SystemDisk.DiskSize,
		Currency:    currency,
		IPv4Public:  first(inst.PublicAddresses),
		IPv4Private: first(inst.PrivateAddresses),
		Tags:        tagStrings(inst.Tags),
		CreatedAt:   parseTime(inst.CreatedTime),
		ExpiresAt:   parseTime(inst.ExpiredTime),
		Image:       tencent.ImageSlug(inst.Platform, inst.OsName),
		// 轻量只有包年包月一种买法：月价就是续一个月的价（套餐目录价），没有小时价
		Billing:   provider.BillingMonthly,
		AutoRenew: autoRenewOf(inst.RenewFlag),
	}
	if price := b.Price.InstancePrice.DiscountPrice; price > 0 {
		s.PriceMonthly = price
	} else {
		s.PriceMonthly = b.Price.InstancePrice.OriginalPrice
	}
	if lat, lng, ok := tencent.RegionGeo(region); ok {
		s.Lat, s.Lng = lat, lng
	}
	return s
}

// autoRenewOf 把实例的续费标识翻成"是否开了自动续费"。平台给的标识有四个取值，看后缀
// 就能分：以 AUTO_RENEW 结尾的是开了，以 MANUAL_RENEW 结尾的是没开；空值（接口没给）
// 返回 nil 表示未知。
func autoRenewOf(flag string) *bool {
	var on bool
	switch {
	case strings.HasSuffix(flag, "AUTO_RENEW"):
		on = true
	case strings.HasSuffix(flag, "MANUAL_RENEW"):
	default:
		return nil
	}
	return &on
}

func first(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

// lighthouseTimeLayouts 是实例时间字段的几种写法（带不带秒 / 小数）。
var lighthouseTimeLayouts = []string{"2006-01-02T15:04:05Z", "2006-01-02T15:04Z", time.RFC3339, "2006-01-02T15:04:05.000Z"}

func parseTime(s string) time.Time {
	for _, layout := range lighthouseTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// tagStrings 把腾讯云的键值标签还原成 vpsctl 的"key:value"扁平串（值为空时只留键）。
func tagStrings(tags []tag) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t.Value == "" {
			out = append(out, t.Key)
		} else {
			out = append(out, t.Key+":"+t.Value)
		}
	}
	return out
}

// tencentTags 是 tagStrings 的逆运算：按第一个冒号切键值，没冒号的整串作键、值留空。
func tencentTags(tags []string) []tag {
	if len(tags) == 0 {
		return nil
	}
	out := make([]tag, 0, len(tags))
	for _, t := range tags {
		key, value, _ := strings.Cut(t, ":")
		if key == "" {
			continue
		}
		out = append(out, tag{Key: key, Value: value})
	}
	return out
}

// sorted 把字符串排序（目录输出用）。
func sorted(ss []string) []string {
	sort.Strings(ss)
	return ss
}
