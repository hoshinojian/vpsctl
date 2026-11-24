package ecs

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hoshinojian/vpsctl/internal/provider"
)

// ---- 地域 ----

// Regions 返回全部地域，slug 即地域 ID（cn-qingdao），名称取阿里云的本地化名。
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
	regions, err := c.regionList(ctx)
	if err != nil || len(regions) == 0 {
		return "", fmt.Errorf("取不到地域表: %v", err)
	}
	return regions[0].ID, nil
}

// ---- 镜像 ----

// publicImages 返回某地域可用的 x86_64 公共 Linux 系统镜像（带缓存）。公共镜像
// 有两三百张，接口每页最多 100，按页取完。
func (c *Client) publicImages(ctx context.Context, region string) ([]image, error) {
	if v, ok := c.images.Get(region); ok {
		return v, nil
	}
	var all []image
	for page := 1; page <= maxPages; page++ {
		var pg struct {
			TotalCount int `json:"TotalCount"`
			Images     struct {
				Image []image `json:"Image"`
			} `json:"Images"`
		}
		err := c.call(ctx, region, "DescribeImages", map[string]string{
			"RegionId": region, "ImageOwnerAlias": "system", "OSType": "linux", "Architecture": "x86_64",
			"PageSize": strconv.Itoa(pageSize), "PageNumber": strconv.Itoa(page),
		}, &pg)
		if err != nil {
			return nil, err
		}
		for _, im := range pg.Images.Image {
			if im.Status == "Available" && strings.Contains(im.Usage, "instance") {
				all = append(all, im)
			}
		}
		if len(pg.Images.Image) == 0 || len(pg.Images.Image) < pageSize || page*pageSize >= pg.TotalCount {
			break
		}
	}
	c.images.Put(region, all)
	return all, nil
}

// imageVersion 抓系统名里的第一个"主版本[.次版本]"，如 "Ubuntu  24.04 64 bit"
// → 24.04。发行版名取 Platform（"Rocky Linux" → rocky-linux）。
var imageVersion = regexp.MustCompile(`(\d+)(?:\.(\d+))?`)

// imageSlug 由镜像的发行版与版本号生成跨地域稳定的 slug：小写发行版名 + 主版本
// [+ 两位次版本]，如 ubuntu-24.04、centos-7.09、rocky-linux-9.08。
func imageSlug(im image) string {
	distro := strings.ToLower(strings.Join(strings.Fields(im.Platform), "-"))
	m := imageVersion.FindStringSubmatch(im.OSNameEn)
	if m == nil {
		return distro
	}
	major, minor := m[1], m[2]
	if len(minor) == 1 {
		minor = "0" + minor
	}
	if minor == "" || minor == "00" {
		return distro + "-" + major
	}
	return distro + "-" + major + "." + minor
}

// imageScore 给同一 slug 下的镜像变体排序，分小者优先：公共镜像里有预装 GPU
// 驱动、Docker 等的变体，vpsctl 要的是最"素"的系统镜像。
func imageScore(im image) int {
	score := 0
	name := im.OSNameEn + " " + im.ImageName
	for _, marker := range []string{"GPU", "CUDA", "Driver", "Docker", "GUI", "LAMP", "LNMP", "NVIDIA", "AIACC", "宝塔", "Node.js"} {
		if strings.Contains(name, marker) {
			score += 10
		}
	}
	return score
}

// catalogImages 把镜像按 slug 归并，每个 slug 留分数最低（并列取最新创建）的一个。
func catalogImages(images []image) map[string]image {
	bySlug := map[string]image{}
	for _, im := range images {
		slug := imageSlug(im)
		cur, ok := bySlug[slug]
		if !ok || imageScore(im) < imageScore(cur) ||
			(imageScore(im) == imageScore(cur) && im.CreationTime > cur.CreationTime) {
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
		out = append(out, provider.Image{Slug: slug, Distribution: im.Platform, Name: im.OSNameEn})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// imageID 把镜像 slug（或直接给的 ImageId）解析成该地域的 ImageId。
func (c *Client) imageID(ctx context.Context, region, slug string) (string, error) {
	images, err := c.publicImages(ctx, region)
	if err != nil {
		return "", err
	}
	if im, ok := catalogImages(images)[slug]; ok {
		return im.ImageId, nil
	}
	// slug 也可能是直接的 ImageId（如 ubuntu_24_04_x64_20G_alibase_20261016.vhd）
	for _, im := range images {
		if im.ImageId == slug {
			return im.ImageId, nil
		}
	}
	slugs := sortedSlugs(images)
	return "", fmt.Errorf("地域 %s 没有镜像 %q（可用: %s；也可直接给公共镜像的 ImageId）",
		region, slug, strings.Join(slugs, ", "))
}

func sortedSlugs(images []image) []string {
	bySlug := catalogImages(images)
	slugs := make([]string, 0, len(bySlug))
	for slug := range bySlug {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return slugs
}

// imageSlugByID 是 imageID 的逆运算：实例的 ImageId → slug；查不到返回空串。
// 按 ImageId 精确查并缓存——列表里每台实例都要走这里，不能拉整个公共镜像列表。
func (c *Client) imageSlugByID(ctx context.Context, region, imageID string) string {
	if imageID == "" {
		return ""
	}
	key := region + "|" + imageID
	if slug, ok := c.slugs.Get(key); ok {
		return slug
	}
	var pg struct {
		Images struct {
			Image []image `json:"Image"`
		} `json:"Images"`
	}
	slug := ""
	err := c.call(ctx, region, "DescribeImages", map[string]string{"RegionId": region, "ImageId": imageID}, &pg)
	if err == nil && len(pg.Images.Image) > 0 {
		slug = imageSlug(pg.Images.Image[0])
	}
	c.slugs.Put(key, slug) // 查不到也缓存空串，免得每次列表都重打
	return slug
}

// ---- 规格 ----

// maxCatalogCPU 是 sizes 目录收录的 vCPU 上限。要更大的机器直接把规格 ID 写进 --size。
const maxCatalogCPU = 8

// catalogSeries 是 sizes 目录收录的规格系列（规格族的字母前缀）：通用型（g）、
// 通用算力型（u）、经济型（e）、突发性能型（t）、内存型（r）、计算型（c）。本地盘、
// GPU、大数据、裸金属等专用规格不在目录里露出。
var catalogSeries = map[string]bool{"g": true, "u": true, "e": true, "t": true, "r": true, "c": true}

var ecsFamilyRe = regexp.MustCompile(`^ecs\.([A-Za-z]+)(\d*)(.*)$`)

// parseFamily 把规格族名拆成"系列 + 代次 + 变体"：ecs.g8a → (g, 8, a)，
// ecs.u1-c1m1 → (u, 1, -c1m1)，ecs.e-c1m1 → (e, 0, -c1m1)。系列为空表示不合命名规则。
func parseFamily(family string) (series string, gen int, variant string) {
	m := ecsFamilyRe.FindStringSubmatch(family)
	if m == nil {
		return "", 0, ""
	}
	if m[2] != "" {
		gen, _ = strconv.Atoi(m[2])
	}
	return m[1], gen, m[3]
}

// latestGeneration 在在售规格里，每个"系列 + 变体 + vCPU + 内存"只留最新一代。
// 同规格新旧几代并存，全列出来有两百多个；同一代里不同的后缀变体（g8a / g8i）都保留。
func latestGeneration(types []instanceType, available map[string]bool) []instanceType {
	type key struct {
		series, variant string
		cpu, mem        int
	}
	best := map[key]int{}
	for _, t := range types {
		series, gen, variant := parseFamily(t.InstanceTypeFamily)
		if !available[t.InstanceTypeId] || !catalogSeries[series] {
			continue
		}
		k := key{series, variant, t.CpuCoreCount, int(t.MemorySize)}
		if gen > best[k] {
			best[k] = gen
		}
	}
	out := make([]instanceType, 0, len(best))
	for _, t := range types {
		series, gen, variant := parseFamily(t.InstanceTypeFamily)
		k := key{series, variant, t.CpuCoreCount, int(t.MemorySize)}
		if available[t.InstanceTypeId] && gen > 0 && gen == best[k] && t.GPUAmount == 0 &&
			t.CpuCoreCount > 0 && t.CpuCoreCount <= maxCatalogCPU {
			out = append(out, t)
		}
	}
	return out
}

// instanceTypes 返回地域的全部规格详情（带缓存）。接口一次就返回全部一千六百个
// 规格，分页参数不生效，约一秒。
func (c *Client) instanceTypes(ctx context.Context, region string) ([]instanceType, error) {
	if v, ok := c.types.Get(region); ok {
		return v, nil
	}
	var out struct {
		InstanceTypes struct {
			InstanceType []instanceType `json:"InstanceType"`
		} `json:"InstanceTypes"`
	}
	if err := c.call(ctx, region, "DescribeInstanceTypes", map[string]string{"RegionId": region}, &out); err != nil {
		return nil, err
	}
	c.types.Put(region, out.InstanceTypes.InstanceType)
	return out.InstanceTypes.InstanceType, nil
}

// availableTypeIDs 返回地域里有库存的在售规格 ID（按量付费、VPC 网络）。
func (c *Client) availableTypeIDs(ctx context.Context, region string) ([]string, error) {
	if v, ok := c.avail.Get(region); ok {
		return v, nil
	}
	// 响应是四层嵌套：可用区 → 资源组 → 支持的资源清单
	type supported struct {
		Status string `json:"Status"`
		Value  string `json:"Value"`
	}
	var out struct {
		AvailableZones struct {
			AvailableZone []struct {
				AvailableResources struct {
					AvailableResource []struct {
						SupportedResources struct {
							SupportedResource []supported `json:"SupportedResource"`
						} `json:"SupportedResources"`
					} `json:"AvailableResource"`
				} `json:"AvailableResources"`
			} `json:"AvailableZone"`
		} `json:"AvailableZones"`
	}
	err := c.call(ctx, region, "DescribeAvailableResource", map[string]string{
		"RegionId": region, "DestinationResource": "InstanceType", "InstanceChargeType": postpaid, "NetworkType": "vpc",
	}, &out)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var ids []string
	for _, z := range out.AvailableZones.AvailableZone {
		for _, g := range z.AvailableResources.AvailableResource {
			for _, r := range g.SupportedResources.SupportedResource {
				if r.Status == "Available" && !seen[r.Value] {
					seen[r.Value] = true
					ids = append(ids, r.Value)
				}
			}
		}
	}
	c.avail.Put(region, ids)
	return ids, nil
}

// Sizes 返回目录地域的在售通用规格（最新一代、无 GPU、vCPU 不超过上限），slug 即
// 规格 ID。阿里云没有批量询价，目录不带月费（月费在节点列表里逐台询）。
func (c *Client) Sizes(ctx context.Context) ([]provider.Size, error) {
	region, err := c.catalogRegion(ctx)
	if err != nil {
		return nil, err
	}
	types, err := c.instanceTypes(ctx, region)
	if err != nil {
		return nil, err
	}
	ids, err := c.availableTypeIDs(ctx, region)
	if err != nil {
		return nil, err
	}
	available := make(map[string]bool, len(ids))
	for _, id := range ids {
		available[id] = true
	}
	out := make([]provider.Size, 0)
	seen := map[string]bool{}
	for _, t := range latestGeneration(types, available) {
		if seen[t.InstanceTypeId] {
			continue
		}
		seen[t.InstanceTypeId] = true
		out = append(out, provider.Size{
			Slug: t.InstanceTypeId, VCPUs: t.CpuCoreCount,
			MemoryMB: int(t.MemorySize * 1024), Currency: currency,
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

// SSHKeys 返回目录地域的密钥对。阿里云建机用密钥对名引用，所以 ID 就是名称。
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
		out = append(out, provider.SSHKey{ID: k.KeyPairName, Name: k.KeyPairName, Fingerprint: k.KeyPairFingerPrint})
	}
	return out, nil
}

func (c *Client) keyPairs(ctx context.Context, region string) ([]keyPair, error) {
	var out struct {
		KeyPairs struct {
			KeyPair []keyPair `json:"KeyPair"`
		} `json:"KeyPairs"`
	}
	// PageSize 上限是 50
	if err := c.call(ctx, region, "DescribeKeyPairs", map[string]string{"RegionId": region, "PageSize": "50"}, &out); err != nil {
		return nil, err
	}
	return out.KeyPairs.KeyPair, nil
}

// resolveKeyPairs 把 --ssh-keys 的输入（密钥对名或指纹）归一为密钥对名。
// 密钥对按地域隔离，只在建机地域内解析。
func (c *Client) resolveKeyPairs(ctx context.Context, region string, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	pairs, err := c.keyPairs(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("解析 ssh key %v: %w", in, err)
	}
	var out []string
	for _, want := range in {
		found := ""
		for _, k := range pairs {
			if k.KeyPairName == want || strings.EqualFold(k.KeyPairFingerPrint, want) {
				found = k.KeyPairName
				break
			}
		}
		if found == "" {
			names := make([]string, 0, len(pairs))
			for _, k := range pairs {
				names = append(names, k.KeyPairName)
			}
			return nil, fmt.Errorf("ssh key %q 不存在于地域 %s（可用密钥对: %s）", want, region, strings.Join(names, ", "))
		}
		out = append(out, found)
	}
	return out, nil
}
