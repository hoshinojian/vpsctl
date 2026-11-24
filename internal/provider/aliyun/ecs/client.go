package ecs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
	"github.com/hoshinojian/vpsctl/internal/provider/aliyun"
)

// ProviderName 是 accounts.json 里 provider 字段的取值。
const ProviderName = "aliyun-ecs"

const (
	version     = "2014-05-26"
	centralHost = "ecs.aliyuncs.com" // 仅全局接口（DescribeRegions）；其余走 ecsHost
	currency    = "CNY"
	postpaid    = "PostPaid"

	pageSize = 100
	maxPages = 1000 // 分页安全上限，防 TotalCount 异常时死循环
	// listConcurrency 是逐地域并发的上限。地域有三十三个，单次调用约一秒，
	// 并发低会让一次列表拖到好几秒；再高则容易撞上接口限流。
	listConcurrency = 16
	// catalogTTL 是镜像 / 规格 / 系统盘 / 询价结果的缓存时长。
	catalogTTL = time.Hour
)

// ecsHost 返回地域专属接入点。地域与实例级接口都必须打到这里：中心接入点
// ecs.aliyuncs.com 不服务其中十四个较新的地域，不带 RegionId 的单实例操作也会
// 报 NotSupportedEndpoint。
func ecsHost(region string) string { return "ecs." + region + ".aliyuncs.com" }

// errNotFound 表示实例在该账号下不存在（所有地域都找不到）。
var errNotFound = errors.New("实例不存在")

type regionInfo struct {
	ID, Name string
}

// Client 是单账号的阿里云 ECS 适配，并发安全。
type Client struct {
	account string
	api     *aliyun.Client

	// 地域表进程内只取一次（地域基本不变），sync.Once 保证并发下也只打一次接口。
	regionsOnce sync.Once
	regions     []regionInfo
	regionsErr  error

	// index 记录实例 ID → 地域（List / Create 时登记），按 ID 操作时先查它。
	index provider.RegionIndex

	images *provider.TTLCache[[]image]        // region → 公共镜像
	types  *provider.TTLCache[[]instanceType] // region → 全部规格详情
	avail  *provider.TTLCache[[]string]       // region → 在售规格 ID
	slugs  *provider.TTLCache[string]         // region|ImageId → slug（列表标注镜像用）
	disks  *provider.TTLCache[disk]           // region|InstanceId → 系统盘
	prices *provider.TTLCache[float64]        // region|可用区|规格|盘类型|盘大小 → 每小时合计价

	// sleep / pollEvery / opWait 供重装链路（关机 → 换盘 → 开机）等待与测试注入
	sleep     func(ctx context.Context, d time.Duration) error
	pollEvery time.Duration
	opWait    time.Duration
}

// New 创建客户端；hc 为 nil 时用带超时的默认客户端。
func New(account, accessKeyID, accessKeySecret string, hc *http.Client) *Client {
	return &Client{
		account: account,
		api:     aliyun.New(accessKeyID, accessKeySecret, hc),
		images:  provider.NewTTLCache[[]image](catalogTTL),
		types:   provider.NewTTLCache[[]instanceType](catalogTTL),
		avail:   provider.NewTTLCache[[]string](catalogTTL),
		slugs:   provider.NewTTLCache[string](catalogTTL),
		disks:   provider.NewTTLCache[disk](catalogTTL),
		prices:  provider.NewTTLCache[float64](catalogTTL),
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
			return nil, errors.New("aliyun-ecs: 缺少 access_key（AccessKey ID）或 secret_key（AccessKey Secret）")
		}
		return New(account, cred.AccessKey, cred.SecretKey, hc), nil
	})
}

// call 发起一次地域内的 ECS 调用。
func (c *Client) call(ctx context.Context, region, action string, params map[string]string, out any) error {
	return c.api.Call(ctx, ecsHost(region), version, action, params, out)
}

// ---- 地域 ----

func (c *Client) regionList(ctx context.Context) ([]regionInfo, error) {
	c.regionsOnce.Do(func() {
		// 首次调用可能带的是短生命周期的 ctx；地域表是一次性数据，用独立 ctx 取，
		// 免得某个调用方取消后缓存下永久失败。
		ctx := context.WithoutCancel(ctx)
		var out struct {
			Regions struct {
				Region []struct {
					RegionId  string `json:"RegionId"`
					LocalName string `json:"LocalName"`
				} `json:"Region"`
			} `json:"Regions"`
		}
		if err := c.api.Call(ctx, centralHost, version, "DescribeRegions", nil, &out); err != nil {
			c.regionsErr = fmt.Errorf("查询地域失败: %w", err)
			return
		}
		for _, r := range out.Regions.Region {
			c.regions = append(c.regions, regionInfo{ID: r.RegionId, Name: r.LocalName})
		}
	})
	return c.regions, c.regionsErr
}

// zoneIDs 返回地域下的可用区 ID。
func (c *Client) zoneIDs(ctx context.Context, region string) ([]string, error) {
	var out struct {
		Zones struct {
			Zone []struct {
				ZoneId string `json:"ZoneId"`
			} `json:"Zone"`
		} `json:"Zones"`
	}
	if err := c.call(ctx, region, "DescribeZones", map[string]string{"RegionId": region}, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Zones.Zone))
	for _, z := range out.Zones.Zone {
		ids = append(ids, z.ZoneId)
	}
	return ids, nil
}

// resolvePlacement 把 create 传进来的区域 slug 解析成地域与可用区。slug 可以是地域
// （cn-qingdao）或该地域下的可用区（cn-qingdao-b）。可用区 ID 的构成不统一——多数
// 是"地域-字母"，也有 us-east-1a 这种直接缀一个字母——所以不按形状猜，一律到
// 该地域的可用区表里核对。
func (c *Client) resolvePlacement(ctx context.Context, slug string) (region, zone string, err error) {
	regions, err := c.regionList(ctx)
	if err != nil {
		return "", "", err
	}
	// 地域 ID 互不为前缀关系（都带各自的数字或地名段），取唯一的前缀匹配即可
	var candidates []regionInfo
	for _, r := range regions {
		if slug == r.ID {
			return r.ID, "", nil
		}
		if len(slug) > len(r.ID) && slug[:len(r.ID)] == r.ID {
			candidates = append(candidates, r)
		}
	}
	for _, r := range candidates {
		zones, err := c.zoneIDs(ctx, r.ID)
		if err != nil {
			return "", "", fmt.Errorf("查询地域 %s 的可用区失败: %w", r.ID, err)
		}
		for _, z := range zones {
			if z == slug {
				return r.ID, z, nil
			}
		}
	}
	return "", "", fmt.Errorf("区域 %q 不是阿里云 ECS 的地域或可用区（可用地域: %s）", slug, joinRegionIDs(regions))
}

func joinRegionIDs(regions []regionInfo) string {
	s := ""
	for i, r := range regions {
		if i > 0 {
			s += ", "
		}
		s += r.ID
	}
	return s
}

// ---- 查询 ----

// List 逐地域并发拉取全部实例并补全标签 / 系统盘 / 月费。任一地域失败即整体
// 失败——编排层的破坏性操作（按 tag 选目标、等删除消失）要求账号下节点必须
// 全量可见，宁可不动手。
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
			insts, err := c.listRegion(ctx, region.ID)
			if err != nil {
				results[i] = result{err: err}
				return
			}
			results[i] = result{servers: c.servers(ctx, region.ID, insts)}
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

// listRegion 分页取一个地域的全部实例，并顺手登记 ID → 地域索引。
func (c *Client) listRegion(ctx context.Context, region string) ([]instance, error) {
	var out []instance
	for page := 1; page <= maxPages; page++ {
		var pg instancesPage
		err := c.call(ctx, region, "DescribeInstances", map[string]string{
			"RegionId": region, "PageSize": strconv.Itoa(pageSize), "PageNumber": strconv.Itoa(page),
		}, &pg)
		if err != nil {
			return nil, err
		}
		for _, inst := range pg.Instances.Instance {
			c.index.Put(inst.InstanceId, region)
		}
		out = append(out, pg.Instances.Instance...)
		if len(pg.Instances.Instance) == 0 || len(out) >= pg.TotalCount {
			return out, nil
		}
	}
	return nil, fmt.Errorf("地域 %s 的实例超过 %d 页，放弃遍历", region, maxPages)
}

// Get 按实例 ID 取详情。地域取自索引，未登记过则逐地域找一次。
func (c *Client) Get(ctx context.Context, id string) (provider.Server, error) {
	region, inst, err := c.lookup(ctx, id)
	if err != nil {
		return provider.Server{}, err
	}
	return c.servers(ctx, region, []instance{inst})[0], nil
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
	insts, err := c.listRegion(ctx, region)
	if err != nil {
		return "", instance{}, err
	}
	inst, err := findInstance(insts, id)
	return region, inst, err
}

// findInstance 在一批实例里按 ID 找；没有则返回包裹 errNotFound 的错误。
func findInstance(insts []instance, id string) (instance, error) {
	for _, inst := range insts {
		if inst.InstanceId == id {
			return inst, nil
		}
	}
	return instance{}, fmt.Errorf("%w: %s", errNotFound, id)
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
			insts, err := c.listRegion(ctx, region)
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
			if _, err := findInstance(insts, id); err != nil {
				return
			}
			select {
			case found <- region:
				cancel() // 找到了，叫停其余地域的查询
			default:
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

// status 把 ECS 的实例状态归一到编排层约定的 active / off / new，其余过渡态
// 小写原样透出（stopping / starting …），由管理台按"其他"着色。
func status(state string) string {
	switch state {
	case "Running":
		return "active"
	case "Stopped":
		return "off"
	case "Pending":
		return "new"
	default:
		return strings.ToLower(state)
	}
}

// servers 把一批同地域的实例映射成统一视图：先取齐标签 / 系统盘 / 镜像名 / 询价，
// 再逐台组装。
func (c *Client) servers(ctx context.Context, region string, insts []instance) []provider.Server {
	if len(insts) == 0 {
		return nil
	}
	tags := c.tagsOf(ctx, region, insts)

	// 每台实例的系统盘各查一次
	disks := make([]disk, len(insts))
	forEachBounded(len(insts), func(i int) { disks[i] = c.systemDisk(ctx, region, insts[i]) })

	// 镜像名与询价按 key 去重：同镜像、同规格同盘的节点各只问一次，结果进缓存共享
	var imageIDs []string
	seenImage := map[string]bool{}
	for _, inst := range insts {
		if inst.ImageId != "" && !seenImage[inst.ImageId] {
			seenImage[inst.ImageId] = true
			if _, ok := c.slugs.Get(region + "|" + inst.ImageId); !ok {
				imageIDs = append(imageIDs, inst.ImageId)
			}
		}
	}
	forEachBounded(len(imageIDs), func(i int) { c.imageSlugByID(ctx, region, imageIDs[i]) })

	var priced []int
	seenPrice := map[string]bool{}
	for i, inst := range insts {
		if inst.InstanceChargeType != postpaid {
			continue
		}
		key := priceKey(region, inst, disks[i])
		if !seenPrice[key] {
			seenPrice[key] = true
			if _, ok := c.prices.Get(key); !ok {
				priced = append(priced, i)
			}
		}
	}
	forEachBounded(len(priced), func(k int) { c.warmPrice(ctx, region, insts[priced[k]], disks[priced[k]]) })

	out := make([]provider.Server, len(insts))
	for i, inst := range insts {
		out[i] = c.toServer(region, inst, tags[inst.InstanceId], disks[i])
	}
	return out
}

// forEachBounded 以至多 listConcurrency 的并发对 0..n-1 各执行一次 fn，全部完成后返回。
func forEachBounded(n int, fn func(i int)) {
	sem := make(chan struct{}, listConcurrency)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// toServer 把实例映射成统一视图。月费只读询价缓存（servers 已预先填好）。
func (c *Client) toServer(region string, inst instance, tags []string, sysDisk disk) provider.Server {
	s := provider.Server{
		ID:          inst.InstanceId,
		Account:     c.account,
		Name:        inst.InstanceName,
		Status:      status(inst.Status),
		Region:      region,
		Size:        inst.InstanceType,
		VCPUs:       inst.Cpu,
		MemoryMB:    inst.Memory,
		DiskGB:      sysDisk.Size,
		Currency:    currency,
		IPv4Private: first(inst.VpcAttributes.PrivateIpAddress.IpAddress),
		Tags:        tags,
		CreatedAt:   parseTime(inst.CreationTime),
		Image:       c.imageSlugCached(region, inst.ImageId),
	}
	s.IPv4Public = first(inst.PublicIpAddress.IpAddress)
	if s.IPv4Public == "" {
		s.IPv4Public = inst.EipAddress.IpAddress // 绑了弹性公网 IP 的实例没有普通公网 IP
	}
	if g, ok := regionGeo[region]; ok {
		s.Lat, s.Lng = g.lat, g.lng
	}
	// 只有按量实例有小时单价；包年包月实例的月费这里算不出来，留空——同规格的按量
	// 实例即使缓存里有价也不能借用
	if inst.InstanceChargeType == postpaid {
		if hourly, ok := c.prices.Get(priceKey(region, inst, sysDisk)); ok && hourly > 0 {
			s.PriceMonthly = hourly * provider.HoursPerMonth
		}
	}
	return s
}

// tagsOf 取一批实例的标签并还原成 vpsctl 的"key:value"扁平串。
// ListTagResources 一次最多查 20 个资源，按地域分批；标签可能被随时增删，不做缓存。
func (c *Client) tagsOf(ctx context.Context, region string, insts []instance) map[string][]string {
	const batch = 20
	out := make(map[string][]string, len(insts))
	for start := 0; start < len(insts); start += batch {
		params := map[string]string{"RegionId": region, "ResourceType": "instance"}
		end := min(start+batch, len(insts))
		for i, inst := range insts[start:end] {
			params[fmt.Sprintf("ResourceId.%d", i+1)] = inst.InstanceId
		}
		var res struct {
			TagResources struct {
				TagResource []tagResource `json:"TagResource"`
			} `json:"TagResources"`
		}
		if err := c.call(ctx, region, "ListTagResources", params, &res); err != nil {
			continue // 标签读不到不该拖垮整张列表：没有标签的节点按 tag 筛不到，不会误伤
		}
		for _, t := range res.TagResources.TagResource {
			if t.TagValue == "" {
				out[t.ResourceId] = append(out[t.ResourceId], t.TagKey)
			} else {
				out[t.ResourceId] = append(out[t.ResourceId], t.TagKey+":"+t.TagValue)
			}
		}
	}
	// 标签的返回顺序没有保证，排一下让两次列表的输出一致
	for id := range out {
		sort.Strings(out[id])
	}
	return out
}

// systemDisk 取实例的系统盘（带缓存）。实例列表不返回磁盘信息，只能逐台问；
// 问不到时返回零值，列表里磁盘列显示为空。
func (c *Client) systemDisk(ctx context.Context, region string, inst instance) disk {
	key := region + "|" + inst.InstanceId
	if d, ok := c.disks.Get(key); ok {
		return d
	}
	var out struct {
		Disks struct {
			Disk []disk `json:"Disk"`
		} `json:"Disks"`
	}
	var sys disk
	err := c.call(ctx, region, "DescribeDisks", map[string]string{
		"RegionId": region, "InstanceId": inst.InstanceId, "DiskType": "system", "PageSize": "100",
	}, &out)
	if err == nil && len(out.Disks.Disk) > 0 {
		sys = out.Disks.Disk[0]
		c.disks.Put(key, sys)
	}
	return sys
}

// priceKey 是询价缓存的键：同一可用区、同一规格、同一系统盘，价格才一样。
func priceKey(region string, inst instance, sysDisk disk) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d", region, inst.ZoneId, inst.InstanceType, sysDisk.Category, sysDisk.Size)
}

// warmPrice 为实例填好询价缓存（已有则不打接口）。
func (c *Client) warmPrice(ctx context.Context, region string, inst instance, sysDisk disk) {
	key := priceKey(region, inst, sysDisk)
	if _, ok := c.prices.Get(key); ok {
		return
	}
	var out priceInfo
	hourly := 0.0
	err := c.call(ctx, region, "DescribePrice", map[string]string{
		"RegionId": region, "ResourceType": "instance", "InstanceType": inst.InstanceType,
		"InstanceChargeType":  postpaid,
		"SystemDisk.Category": sysDisk.Category, "SystemDisk.Size": strconv.Itoa(sysDisk.Size),
	}, &out)
	if err == nil {
		hourly = out.PriceInfo.Price.TradePrice // 每小时合计：规格 + 系统盘
	}
	c.prices.Put(key, hourly) // 问不到也缓存 0，免得每次列表都重打
}

// imageSlugCached 只读缓存取镜像 slug（由 servers 预先填好），本函数不发请求。
func (c *Client) imageSlugCached(region, imageID string) string {
	slug, _ := c.slugs.Get(region + "|" + imageID)
	return slug
}

func first(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

// aliyunTimeLayouts 是 ECS 时间字段的几种写法（有的不带秒）。
var aliyunTimeLayouts = []string{"2006-01-02T15:04:05Z", "2006-01-02T15:04Z", time.RFC3339}

func parseTime(s string) time.Time {
	for _, layout := range aliyunTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
