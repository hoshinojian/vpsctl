package cvm

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
const ProviderName = "tencent-cvm"

const (
	host       = "cvm.tencentcloudapi.com"
	service    = "cvm"
	version    = "2017-03-12"
	vpcHost    = "vpc.tencentcloudapi.com"
	vpcService = "vpc"

	currency = "CNY"
	postpaid = "POSTPAID_BY_HOUR"

	pageSize = 100
	// listConcurrency 是逐地域并发的上限。地域数不到二十，单次调用约一秒，
	// 并发低会让一次列表拖到好几秒；再高则容易撞上接口限流。
	listConcurrency = 12
	// catalogTTL 是镜像 / 机型 / 询价结果的缓存时长。
	catalogTTL = time.Hour
)

// errNotFound 表示实例在该账号下不存在（所有地域都找不到）。
var errNotFound = errors.New("实例不存在")

// Client 是单账号的腾讯云 CVM 适配，并发安全。
type Client struct {
	account string
	api     *tencent.Client

	// 地域表进程内只取一次（地域基本不变），sync.Once 保证并发下也只打一次接口。
	regionsOnce sync.Once
	regionIDs   []string
	regionNames map[string]string
	regionsErr  error

	// index 记录实例 ID → 地域（List / Create 时登记），按 ID 操作时先查它。
	index provider.RegionIndex

	images     *provider.TTLCache[[]image] // region → 公共镜像（建机 / 重装解析 slug 用）
	slugs      *provider.TTLCache[string]  // region|ImageId → slug（列表标注镜像用）
	typeConfig *provider.TTLCache[[]instanceTypeConfig]
	prices     *provider.TTLCache[float64] // region|可用区|机型 → 按量小时单价

	// sleep / pollEvery / opWait 供重装链路（关机 → 重装 → 开机）等待与测试注入
	sleep     func(ctx context.Context, d time.Duration) error
	pollEvery time.Duration
	opWait    time.Duration
}

// New 创建客户端；hc 为 nil 时用带超时的默认客户端。
func New(account, secretID, secretKey string, hc *http.Client) *Client {
	return &Client{
		account:    account,
		api:        tencent.New(secretID, secretKey, hc),
		images:     provider.NewTTLCache[[]image](catalogTTL),
		slugs:      provider.NewTTLCache[string](catalogTTL),
		typeConfig: provider.NewTTLCache[[]instanceTypeConfig](catalogTTL),
		prices:     provider.NewTTLCache[float64](catalogTTL),
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
			return nil, errors.New("tencent-cvm: 缺少 access_key（SecretId）或 secret_key（SecretKey）")
		}
		return New(account, cred.AccessKey, cred.SecretKey, hc), nil
	})
	// 公共镜像的 cloud-init 可能不认 chpasswd.users：密码要兼容老写法下发
	provider.RegisterChpasswdCompat(ProviderName)
}

// ---- 地域 ----

// regionList 返回全部地域 ID（接口返回序）与"地域 ID → 区域名"。
func (c *Client) regionList(ctx context.Context) ([]string, map[string]string, error) {
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
		c.regionNames = make(map[string]string, len(out.RegionSet))
		for _, r := range out.RegionSet {
			c.regionIDs = append(c.regionIDs, r.Region)
			c.regionNames[r.Region] = r.RegionName
		}
	})
	return c.regionIDs, c.regionNames, c.regionsErr
}

// resolvePlacement 把 create 传进来的区域 slug 解析成地域与可用区。slug 可以是
// 地域（ap-beijing）或该地域下的可用区（ap-beijing-8）——可用区 ID 一定是
// "地域ID-序号"，按最长前缀匹配，避免 ap-beijing 抢了 ap-beijing-8 的前缀。
func (c *Client) resolvePlacement(ctx context.Context, slug string) (region, zone string, err error) {
	ids, _, err := c.regionList(ctx)
	if err != nil {
		return "", "", err
	}
	sorted := append([]string(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, r := range sorted {
		switch {
		case slug == r:
			return r, "", nil
		case strings.HasPrefix(slug, r+"-"):
			return r, slug, nil
		}
	}
	return "", "", fmt.Errorf("区域 %q 不是腾讯云 CVM 的地域或可用区（可用: %s）", slug, strings.Join(ids, ", "))
}

// ---- 查询 ----

// List 逐地域并发拉取全部实例。任一地域失败即整体失败——编排层的破坏性操作
// （按 tag 选目标、等删除消失）要求账号下节点必须全量可见，宁可不动手。
func (c *Client) List(ctx context.Context) ([]provider.Server, error) {
	regions, _, err := c.regionList(ctx)
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
		go func(i int, region string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			servers, err := c.listRegion(ctx, region)
			results[i] = result{servers, err}
		}(i, region)
	}
	wg.Wait()
	out := make([]provider.Server, 0)
	for i, r := range results {
		if r.err != nil {
			return nil, fmt.Errorf("地域 %s: %w", regions[i], r.err)
		}
		out = append(out, r.servers...)
	}
	return out, nil
}

func (c *Client) listRegion(ctx context.Context, region string) ([]provider.Server, error) {
	var out []provider.Server
	for offset := 0; ; offset += pageSize {
		var page instancesPage
		err := c.api.Call(ctx, host, service, version, "DescribeInstances", region,
			map[string]any{"Limit": pageSize, "Offset": offset}, &page)
		if err != nil {
			return nil, err
		}
		for i := range page.InstanceSet {
			inst := page.InstanceSet[i]
			c.index.Put(inst.InstanceId, region)
		}
		// 先并发把这一页的询价缓存填好，toServer 才不用逐台串行打接口
		c.warmPrices(ctx, region, page.InstanceSet)
		for i := range page.InstanceSet {
			s := c.toServer(region, page.InstanceSet[i])
			s.Image = c.imageSlugByID(ctx, region, page.InstanceSet[i].ImageId)
			out = append(out, s)
		}
		if len(page.InstanceSet) == 0 || offset+len(page.InstanceSet) >= page.TotalCount || offset >= maxListOffset {
			return out, nil
		}
	}
}

// maxListOffset 是分页安全上限，防 TotalCount 异常时死循环。
const maxListOffset = 100000

// Get 按实例 ID 取详情。地域取自索引，未登记过则逐地域找一次。
func (c *Client) Get(ctx context.Context, id string) (provider.Server, error) {
	region, inst, err := c.lookup(ctx, id)
	if err != nil {
		return provider.Server{}, err
	}
	c.warmPrices(ctx, region, []instance{inst})
	s := c.toServer(region, inst)
	s.Image = c.imageSlugByID(ctx, region, inst.ImageId)
	return s, nil
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
	inst, err := c.describe(ctx, region, id)
	return region, inst, err
}

// locate 在所有地域里并发找一台实例（索引未命中时的兜底）；找到即登记并
// 取消其余查询。
func (c *Client) locate(ctx context.Context, id string) (string, error) {
	regions, _, err := c.regionList(ctx)
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
	for _, region := range regions {
		wg.Add(1)
		go func(region string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if _, err := c.describe(ctx, region, id); err != nil {
				// "这个地域没有这台机器"是正常结果，只有真正的查询失败才记下来
				if !errors.Is(err, errNotFound) && ctx.Err() == nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
				return
			}
			select {
			case found <- region:
				cancel() // 找到了，叫停其余地域的查询
			default:
			}
		}(region)
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

// describe 在指定地域按实例 ID 精确查一台；查不到返回包裹 errNotFound 的错误。
func (c *Client) describe(ctx context.Context, region, id string) (instance, error) {
	var page instancesPage
	err := c.api.Call(ctx, host, service, version, "DescribeInstances", region, map[string]any{
		"Filters": []map[string]any{{"Name": "instance-id", "Values": []string{id}}},
	}, &page)
	if err != nil {
		return instance{}, err
	}
	if len(page.InstanceSet) == 0 {
		return instance{}, fmt.Errorf("%w: %s", errNotFound, id)
	}
	return page.InstanceSet[0], nil
}

// status 把 CVM 的 InstanceState 归一到编排层约定的 active / off / new，
// 其余过渡态小写原样透出（stopping / starting / rebooting …），由管理台按"其他"着色。
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

// toServer 把实例映射成统一视图。月费只读询价缓存（由 warmPrices / Get 预先填好），
// 本函数不发请求。
func (c *Client) toServer(region string, inst instance) provider.Server {
	s := provider.Server{
		ID:          inst.InstanceId,
		Account:     c.account,
		Name:        inst.InstanceName,
		Status:      status(inst.InstanceState),
		Region:      region,
		Size:        inst.InstanceType,
		VCPUs:       inst.CPU,
		MemoryMB:    inst.Memory * 1024,
		DiskGB:      inst.SystemDisk.DiskSize,
		Currency:    currency,
		IPv4Public:  first(inst.PublicIpAddresses),
		IPv4Private: first(inst.PrivateIpAddresses),
		Tags:        tagStrings(inst.Tags),
		CreatedAt:   parseTime(inst.CreatedTime),
	}
	if g, ok := regionGeo[region]; ok {
		s.Lat, s.Lng = g.lat, g.lng
	}
	// 只有按量实例能问到小时单价；包年包月实例的月费这里算不出来，留空
	if inst.InstanceChargeType == postpaid && inst.InstanceType != "" {
		if hourly, ok := c.prices.Get(region + "|" + inst.Placement.Zone + "|" + inst.InstanceType); ok && hourly > 0 {
			s.PriceMonthly = hourly * provider.HoursPerMonth
		}
	}
	return s
}

func first(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
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
func tencentTags(tags []string) []map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]map[string]string, 0, len(tags))
	for _, t := range tags {
		key, value, _ := strings.Cut(t, ":")
		if key == "" {
			continue
		}
		out = append(out, map[string]string{"Key": key, "Value": value})
	}
	return out
}
