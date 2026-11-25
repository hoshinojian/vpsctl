package cvm

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/hoshinojian/vpsctl/internal/provider"
	"github.com/hoshinojian/vpsctl/internal/provider/tencent"
)

// ---- 地域 ----

// Regions 返回全部地域，slug 即地域 ID（ap-beijing）。
func (c *Client) Regions(ctx context.Context) ([]provider.Region, error) {
	ids, names, err := c.regionList(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Region, 0, len(ids))
	for _, id := range ids {
		out = append(out, provider.Region{Slug: id, Name: names[id]})
	}
	return out, nil
}

// catalogRegion 决定目录类查询（sizes / images / keys）在哪个地域取样：节点最多
// 的地域，一台节点都没有则取地域表的第一个。
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
	ids, _, err := c.regionList(ctx)
	if err != nil || len(ids) == 0 {
		return "", fmt.Errorf("取不到地域表: %v", err)
	}
	return ids[0], nil
}

// ---- 镜像 ----

// publicImages 返回某地域可用的 Linux 公共镜像（带缓存）。
func (c *Client) publicImages(ctx context.Context, region string) ([]image, error) {
	if v, ok := c.images.Get(region); ok {
		return v, nil
	}
	var all []image
	for offset := 0; ; offset += pageSize {
		var page struct {
			TotalCount int     `json:"TotalCount"`
			ImageSet   []image `json:"ImageSet"`
		}
		err := c.api.Call(ctx, host, service, version, "DescribeImages", region, map[string]any{
			"Limit":   pageSize,
			"Offset":  offset,
			"Filters": []map[string]any{{"Name": "image-type", "Values": []string{"PUBLIC_IMAGE"}}},
		}, &page)
		if err != nil {
			return nil, err
		}
		for _, im := range page.ImageSet {
			if im.OsType == "linux" && im.ImageState == "NORMAL" && plainOS(im) {
				all = append(all, im)
			}
		}
		if len(page.ImageSet) == 0 || offset+len(page.ImageSet) >= page.TotalCount {
			break
		}
	}
	c.images.Put(region, all)
	return all, nil
}

// plainOS 判断公共镜像是不是"纯系统镜像"。腾讯云把预装了应用的镜像也归在公共
// 镜像里，名字形如 "DeepSeek Harness on Ubuntu24.04"；纯系统镜像的名字以发行版
// 名开头（"Ubuntu Server 24.04 LTS 64bit"）。
func plainOS(im image) bool {
	return im.Platform != "" && strings.HasPrefix(strings.ToLower(im.ImageName), strings.ToLower(im.Platform))
}

// imageScore 给同一 slug 下的镜像变体排序，分小者优先：同版本常有 ARM、UEFI、
// 预装 GPU 驱动等变体，vpsctl 要的是最"素"的 x86_64 系统镜像。
func imageScore(im image) int {
	score := 0
	if im.Architecture != "x86_64" {
		score += 100
	}
	for _, marker := range []string{"UEFI", "HCC", "GPU", "CUDA", "ARM", "Docker", "docker"} {
		if strings.Contains(im.ImageName, marker) || strings.Contains(im.Platform, marker) {
			score += 10
		}
	}
	return score
}

// catalogImages 把纯系统镜像按 slug 归并，每个 slug 留分数最低（并列取名字序最小）
// 的一个。预装应用的镜像在这里就被排除，不依赖调用方先过滤。
func catalogImages(images []image) map[string]image {
	bySlug := map[string]image{}
	for _, im := range images {
		if !plainOS(im) {
			continue
		}
		slug := tencent.ImageSlug(im.Platform, im.ImageName)
		cur, ok := bySlug[slug]
		if !ok || imageScore(im) < imageScore(cur) ||
			(imageScore(im) == imageScore(cur) && im.ImageName < cur.ImageName) {
			bySlug[slug] = im
		}
	}
	return bySlug
}

// Images 返回目录地域的镜像，slug 为 imageSlug，发行版取 Platform。
func (c *Client) Images(ctx context.Context) ([]provider.Image, error) {
	region, err := c.catalogRegion(ctx)
	if err != nil {
		return nil, err
	}
	images, err := c.publicImages(ctx, region)
	if err != nil {
		return nil, err
	}
	bySlug := catalogImages(images)
	out := make([]provider.Image, 0, len(bySlug))
	for slug, im := range bySlug {
		out = append(out, provider.Image{Slug: slug, Distribution: im.Platform, Name: im.ImageName})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// imageID 把镜像 slug（或直接给的 ImageId）解析成该地域的 ImageId。
func (c *Client) imageID(ctx context.Context, region, slug string) (string, error) {
	if strings.HasPrefix(slug, "img-") {
		return slug, nil
	}
	images, err := c.publicImages(ctx, region)
	if err != nil {
		return "", err
	}
	if im, ok := catalogImages(images)[slug]; ok {
		return im.ImageId, nil
	}
	slugs := make([]string, 0, len(images))
	for slug := range catalogImages(images) {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return "", fmt.Errorf("地域 %s 没有镜像 %q（可用: %s；也可直接给 img- 开头的 ImageId）",
		region, slug, strings.Join(slugs, ", "))
}

// imageSlugByID 是 imageID 的逆运算：实例的 ImageId → slug；查不到返回空串。
// 按 ImageId 精确查并缓存——列表里每台实例都要走这里，不能像建机路径那样
// 拉整个公共镜像列表（几百条、分好几页）。
func (c *Client) imageSlugByID(ctx context.Context, region, imageID string) string {
	if imageID == "" {
		return ""
	}
	key := region + "|" + imageID
	if slug, ok := c.slugs.Get(key); ok {
		return slug
	}
	var page struct {
		ImageSet []image `json:"ImageSet"`
	}
	slug := ""
	err := c.api.Call(ctx, host, service, version, "DescribeImages", region, map[string]any{
		"Limit":   1,
		"Filters": []map[string]any{{"Name": "image-id", "Values": []string{imageID}}},
	}, &page)
	if err == nil && len(page.ImageSet) > 0 {
		slug = tencent.ImageSlug(page.ImageSet[0].Platform, page.ImageSet[0].ImageName)
	}
	c.slugs.Put(key, slug) // 查不到也缓存空串，免得每次列表都重打
	return slug
}

// ---- 机型 ----

// maxCatalogCPU 是 sizes 目录收录的 vCPU 上限。要更大的机器直接把机型名写进 --size。
const maxCatalogCPU = 8

// catalogSeries 是 sizes 目录收录的机型系列（实例族的字母前缀）：标准型（S / SA）、
// 突发性能型（IT）、内存型（M / MA）、计算型（C / CA）。大数据、GPU、裸金属等专用
// 机型不在目录里露出，需要时直接把机型名写进 --size。
var catalogSeries = map[string]bool{"S": true, "SA": true, "IT": true, "M": true, "MA": true, "C": true, "CA": true}

var familyRe = regexp.MustCompile(`^([A-Z]+)(\d+)`)

// latestGeneration 每个系列只留最新一代机型。同规格新旧几代并存（S2 / S3 / S4 /
// S5…），全列出来有两百多个；同一代里带后缀的变体（如 S6 与 S6e）都保留。
func latestGeneration(types []instanceTypeConfig) []instanceTypeConfig {
	type key struct {
		series   string
		cpu, mem int
	}
	bestGen := map[key]int{}
	for _, t := range types {
		series, gen := parseFamily(t.InstanceFamily)
		if series == "" || !catalogSeries[series] {
			continue
		}
		k := key{series, t.CPU, t.Memory}
		if gen > bestGen[k] {
			bestGen[k] = gen
		}
	}
	out := make([]instanceTypeConfig, 0, len(bestGen))
	for _, t := range types {
		series, gen := parseFamily(t.InstanceFamily)
		k := key{series, t.CPU, t.Memory}
		if series != "" && gen > 0 && gen == bestGen[k] {
			out = append(out, t)
		}
	}
	return out
}

// parseFamily 把实例族名拆成字母系列与数字代次：SA5 → ("SA", 5)，IT5 → ("IT", 5)。
func parseFamily(family string) (string, int) {
	m := familyRe.FindStringSubmatch(family)
	if m == nil {
		return "", 0
	}
	gen := 0
	for _, r := range m[2] {
		gen = gen*10 + int(r-'0')
	}
	return m[1], gen
}

func (c *Client) instanceTypes(ctx context.Context, region string) ([]instanceTypeConfig, error) {
	if v, ok := c.typeConfig.Get(region); ok {
		return v, nil
	}
	var out struct {
		InstanceTypeConfigSet []instanceTypeConfig `json:"InstanceTypeConfigSet"`
	}
	if err := c.api.Call(ctx, host, service, version, "DescribeInstanceTypeConfigs", region, nil, &out); err != nil {
		return nil, err
	}
	c.typeConfig.Put(region, out.InstanceTypeConfigSet)
	return out.InstanceTypeConfigSet, nil
}

// Sizes 返回目录地域的通用机型（最新一代、无 GPU/FPGA、vCPU 不超过上限），slug 即
// 机型名。接口按可用区重复返回同一机型，这里去重。CVM 没有批量询价，目录不带
// 月费（月费在节点列表里逐台询）。
func (c *Client) Sizes(ctx context.Context) ([]provider.Size, error) {
	region, err := c.catalogRegion(ctx)
	if err != nil {
		return nil, err
	}
	types, err := c.instanceTypes(ctx, region)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]provider.Size, 0)
	for _, t := range latestGeneration(types) {
		if seen[t.InstanceType] || t.GPU != 0 || t.FPGA != 0 || t.CPU <= 0 || t.CPU > maxCatalogCPU {
			continue
		}
		seen[t.InstanceType] = true
		out = append(out, provider.Size{
			Slug: t.InstanceType, VCPUs: t.CPU, MemoryMB: t.Memory * 1024, Currency: currency,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.VCPUs != b.VCPUs {
			return a.VCPUs < b.VCPUs
		}
		if a.MemoryMB != b.MemoryMB {
			return a.MemoryMB < b.MemoryMB
		}
		return a.Slug < b.Slug
	})
	return out, nil
}

// ---- 密钥对 ----

// SSHKeys 返回目录地域的密钥对。腾讯云不返回指纹，这里按 OpenSSH 的算法
// （公钥 base64 体的 MD5）现算一个，格式与 DO 的指纹一致，方便 --ssh-keys 里
// 直接写指纹。
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

// resolveSSHKeys 把混合输入（skey- 密钥对 ID / 名称 / 指纹）归一为密钥对 ID。
// 密钥对按地域隔离，只在建机地域内解析。
func (c *Client) resolveSSHKeys(ctx context.Context, region string, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	var out, toResolve []string
	for _, k := range in {
		if strings.HasPrefix(k, "skey-") {
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
			names := make([]string, 0, len(pairs))
			for _, k := range pairs {
				names = append(names, k.KeyName)
			}
			return nil, fmt.Errorf("ssh key %q 不存在于地域 %s（可用密钥对: %s）", want, region, strings.Join(names, ", "))
		}
		out = append(out, found)
	}
	sort.Strings(out)
	return out, nil
}

// ---- 询价 ----

// hourlyPrice 返回实例的按量小时单价（折后价优先），结果带缓存。询价接口必须
// 给全镜像、可用区、系统盘才能出价，所以入参是实例本身；问不到时缓存 0 并
// 返回 false——月费展示不出来不应拖垮列表。
//
// 例外：实例不在稳定态（还在创建、开关机、重装）时，问不到价不缓存。管理台建完
// 机器后每几秒刷一次列表，创建中那次失败要是缓存了，机器都跑起来了价格列还要
// 再显示一小时"询不到价"（2026-11-25 管理台建机实测）。
func (c *Client) hourlyPrice(ctx context.Context, region string, inst instance) (float64, bool) {
	key := region + "|" + inst.Placement.Zone + "|" + inst.InstanceType
	if p, ok := c.prices.Get(key); ok {
		return p, p > 0
	}
	var out struct {
		Price *struct {
			InstancePrice *hourlyPrice `json:"InstancePrice"`
		} `json:"Price"`
	}
	hourly := 0.0
	err := c.api.Call(ctx, host, service, version, "InquiryPriceRunInstances", region, map[string]any{
		"InstanceChargeType": postpaid,
		"InstanceType":       inst.InstanceType,
		"InstanceCount":      1,
		"ImageId":            inst.ImageId,
		"Placement":          map[string]any{"Zone": inst.Placement.Zone},
		"SystemDisk":         map[string]any{"DiskType": inst.SystemDisk.DiskType, "DiskSize": inst.SystemDisk.DiskSize},
	}, &out)
	if err == nil && out.Price != nil && out.Price.InstancePrice != nil {
		hourly = out.Price.InstancePrice.UnitPriceDiscount
		if hourly <= 0 {
			hourly = out.Price.InstancePrice.UnitPrice
		}
	}
	if hourly <= 0 && !priceSettled(inst) {
		return 0, false
	}
	c.prices.Put(key, hourly)
	return hourly, hourly > 0
}

// priceSettled 表示实例不处在任何操作的中间态——只有运行中和已关机两种。
func priceSettled(inst instance) bool {
	return inst.InstanceState == "RUNNING" || inst.InstanceState == "STOPPED"
}

// warmPrices 并发预热一批实例的询价缓存，让随后的 toServer 只读缓存不再串行打接口。
func (c *Client) warmPrices(ctx context.Context, region string, insts []instance) {
	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, priceConcurrency)
	)
	for _, inst := range insts {
		if inst.InstanceChargeType != postpaid || inst.InstanceType == "" {
			continue
		}
		if _, ok := c.prices.Get(region + "|" + inst.Placement.Zone + "|" + inst.InstanceType); ok {
			continue
		}
		wg.Add(1)
		go func(inst instance) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_, _ = c.hourlyPrice(ctx, region, inst)
		}(inst)
	}
	wg.Wait()
}

// priceConcurrency 是询价并发上限（询价接口限流较紧）。
const priceConcurrency = 4
