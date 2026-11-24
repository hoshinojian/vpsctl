package lighthouse

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/hoshinojian/vpsctl/internal/provider"
	"github.com/hoshinojian/vpsctl/internal/provider/tencent"
)

// catalogRegion 决定目录类查询（sizes / images / keys）在哪个地域取样：节点最多的
// 地域，一台节点都没有则取地域表的第一个。
func (c *Client) catalogRegion(ctx context.Context) (string, error) {
	servers, err := c.List(ctx)
	if err != nil {
		return "", err
	}
	counts := map[string]int{}
	for _, s := range servers {
		counts[s.Region]++
	}
	best := ""
	for region, n := range counts {
		// 节点数相同时取地域 ID 字典序小的，保证每次给出同一个目录地域
		if best == "" || n > counts[best] || (n == counts[best] && region < best) {
			best = region
		}
	}
	if best != "" {
		return best, nil
	}
	regions, err := c.regionList(ctx)
	if err != nil || len(regions) == 0 {
		return "", fmt.Errorf("取不到地域表: %v", err)
	}
	return regions[0].ID, nil
}

// ---- 地域 ----

// Regions 返回全部地域，slug 即地域 ID（ap-guangzhou）。
func (c *Client) Regions(ctx context.Context) ([]provider.Region, error) {
	regions, err := c.regionList(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Region, 0, len(regions))
	for _, r := range regions {
		out = append(out, provider.Region{Slug: r.ID, Name: r.Name})
	}
	return out, nil
}

// ---- 套餐 ----

// bundlesOf 返回地域的套餐表（BundleId → 套餐，带月价），带缓存。
func (c *Client) bundlesOf(ctx context.Context, region string) (map[string]bundle, error) {
	if v, ok := c.bundles.Get(region); ok {
		return v, nil
	}
	var out struct {
		BundleSet []bundle `json:"BundleSet"`
	}
	if err := c.api.Call(ctx, host, service, version, "DescribeBundles", region, map[string]any{"Limit": pageSize}, &out); err != nil {
		return nil, err
	}
	byID := make(map[string]bundle, len(out.BundleSet))
	for _, b := range out.BundleSet {
		byID[b.BundleId] = b
	}
	c.bundles.Put(region, byID)
	return byID, nil
}

// Sizes 返回目录地域在售的 Linux 套餐，slug 即套餐 ID，按月价升序。轻量应用服务器的
// 套餐自带月价，不需要逐台询价。
func (c *Client) Sizes(ctx context.Context) ([]provider.Size, error) {
	region, err := c.catalogRegion(ctx)
	if err != nil {
		return nil, err
	}
	byID, err := c.bundlesOf(ctx, region)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Size, 0, len(byID))
	for _, b := range byID {
		if b.BundleSalesState != "AVAILABLE" || !b.SupportLinuxUnixPlatform {
			continue
		}
		out = append(out, provider.Size{
			Slug: b.BundleId, VCPUs: b.CPU, MemoryMB: b.Memory * 1024, DiskGB: b.SystemDiskSize,
			PriceMonthly: monthlyPrice(b), Currency: currency,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.PriceMonthly != b.PriceMonthly {
			return a.PriceMonthly < b.PriceMonthly
		}
		return a.Slug < b.Slug
	})
	return out, nil
}

func monthlyPrice(b bundle) float64 {
	if p := b.Price.InstancePrice.DiscountPrice; p > 0 {
		return p
	}
	return b.Price.InstancePrice.OriginalPrice
}

// ---- 镜像 ----

// blueprintsOf 返回地域的全部镜像（带缓存），按页取完。
func (c *Client) blueprintsOf(ctx context.Context, region string) ([]blueprint, error) {
	if v, ok := c.blueprints.Get(region); ok {
		return v, nil
	}
	var all []blueprint
	for offset := 0; offset < maxPages*pageSize; offset += pageSize {
		var page struct {
			TotalCount   int         `json:"TotalCount"`
			BlueprintSet []blueprint `json:"BlueprintSet"`
		}
		err := c.api.Call(ctx, host, service, version, "DescribeBlueprints", region, map[string]any{"Limit": pageSize, "Offset": offset}, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page.BlueprintSet...)
		if len(page.BlueprintSet) == 0 || offset+len(page.BlueprintSet) >= page.TotalCount {
			break
		}
	}
	c.blueprints.Put(region, all)
	return all, nil
}

// osBlueprints 筛出可用于建机的纯系统镜像：类型是 PURE_OS、Linux、状态正常。
// APP_OS / DOCKER 类型是预装了应用的镜像，不当系统镜像用。
func osBlueprints(all []blueprint) []blueprint {
	out := make([]blueprint, 0, len(all))
	for _, b := range all {
		if b.BlueprintType == "PURE_OS" && b.PlatformType == "LINUX_UNIX" && b.BlueprintState == "NORMAL" {
			out = append(out, b)
		}
	}
	return out
}

// Images 返回目录地域的纯系统镜像，slug 与云服务器（CVM）同一套（ubuntu-24.04…）。
func (c *Client) Images(ctx context.Context) ([]provider.Image, error) {
	region, err := c.catalogRegion(ctx)
	if err != nil {
		return nil, err
	}
	all, err := c.blueprintsOf(ctx, region)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]provider.Image, 0)
	for _, b := range osBlueprints(all) {
		slug := tencent.ImageSlug(b.Platform, b.OsName)
		if seen[slug] {
			continue
		}
		seen[slug] = true
		out = append(out, provider.Image{Slug: slug, Distribution: b.Platform, Name: b.OsName})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// blueprintID 把镜像 slug（或直接给的 BlueprintId）解析成该地域的镜像 ID。
func (c *Client) blueprintID(ctx context.Context, region, slug string) (string, error) {
	if strings.HasPrefix(slug, "lhbp-") {
		return slug, nil
	}
	all, err := c.blueprintsOf(ctx, region)
	if err != nil {
		return "", err
	}
	os := osBlueprints(all)
	for _, b := range os {
		if tencent.ImageSlug(b.Platform, b.OsName) == slug {
			return b.BlueprintId, nil
		}
	}
	slugs := make([]string, 0, len(os))
	for _, b := range os {
		slugs = append(slugs, tencent.ImageSlug(b.Platform, b.OsName))
	}
	slices.Sort(slugs)
	return "", fmt.Errorf("地域 %s 没有系统镜像 %q（可用: %s；也可直接给 lhbp- 开头的镜像 ID）",
		region, slug, strings.Join(slugs, ", "))
}

// ---- 密钥对 ----

// SSHKeys 返回目录地域的密钥对；腾讯云不返回指纹，这里按 OpenSSH 的算法现算一个。
func (c *Client) SSHKeys(ctx context.Context) ([]provider.SSHKey, error) {
	region, err := c.catalogRegion(ctx)
	if err != nil {
		return nil, err
	}
	pairs, err := c.keyPairs(ctx, region)
	if err != nil {
		return nil, err
	}
	out := make([]provider.SSHKey, 0, len(pairs))
	for _, k := range pairs {
		out = append(out, provider.SSHKey{ID: k.KeyPairId, Name: k.KeyName, Fingerprint: tencent.SSHFingerprint(k.PublicKey)})
	}
	return out, nil
}

func (c *Client) keyPairs(ctx context.Context, region string) ([]keyPair, error) {
	var out struct {
		KeyPairSet []keyPair `json:"KeyPairSet"`
	}
	if err := c.api.Call(ctx, host, service, version, "DescribeKeyPairs", region, map[string]any{"Limit": pageSize}, &out); err != nil {
		return nil, err
	}
	return out.KeyPairSet, nil
}

// resolveKeys 把混合输入（lhkp- 密钥对 ID / 名称 / 指纹）归一为密钥对 ID。
func (c *Client) resolveKeys(ctx context.Context, region string, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	var out, toResolve []string
	for _, k := range in {
		if strings.HasPrefix(k, "lhkp-") {
			out = append(out, k)
		} else {
			toResolve = append(toResolve, k)
		}
	}
	if len(toResolve) == 0 {
		return out, nil
	}
	pairs, err := c.keyPairs(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("解析 ssh key 名称 %v: %w", toResolve, err)
	}
	for _, want := range toResolve {
		found := ""
		for _, k := range pairs {
			if k.KeyName == want || tencent.SSHFingerprint(k.PublicKey) == want {
				found = k.KeyPairId
				break
			}
		}
		if found == "" {
			return nil, fmt.Errorf("ssh key %q 不存在于地域 %s", want, region)
		}
		out = append(out, found)
	}
	return out, nil
}
