package ecs

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
	"github.com/hoshinojian/vpsctl/internal/provider/aliyun"
)

// create.go：建机。按量付费（与 DO 的按小时同语义），落在该地域的（默认优先）
// 交换机上，安全组取交换机所在 VPC 里的第一个。

const (
	// defaultSystemDiskGB / defaultBandwidthMbps 是建机缺省的系统盘大小与公网带宽
	// 上限。公网按流量计费，带宽上限本身不产生费用；公共镜像 20G，ESSD 最小 40G。
	defaultSystemDiskGB   = 40
	defaultBandwidthMbps  = "10"
	maxTagsPerCreate      = 20 // RunInstances 单次最多带 20 对标签
	createVisibleWait     = 30 * time.Second
	createVisibleInterval = time.Second
)

// diskCandidates 是系统盘云盘类型的尝试顺序。各规格支持的云盘类型不一（突发性能型
// 不支持 ESSD），又没有省事的查询口，建机前用 DryRun 逐个试，取第一个通过的。
var diskCandidates = []string{"cloud_essd", "cloud_essd_entry", "cloud_efficiency", "cloud_auto", "cloud_ssd"}

// Create 提交建机并立即返回（实例随后进入 new → active）。
func (c *Client) Create(ctx context.Context, req provider.CreateRequest) (provider.Server, error) {
	region, zoneHint, err := c.resolvePlacement(ctx, req.Region)
	if err != nil {
		return provider.Server{}, err
	}
	imageID, err := c.imageID(ctx, region, req.Image)
	if err != nil {
		return provider.Server{}, fmt.Errorf("解析镜像: %w", err)
	}
	vswID, sgID, zone, err := c.defaultNetwork(ctx, region, zoneHint)
	if err != nil {
		return provider.Server{}, err
	}
	keys, err := c.resolveKeyPairs(ctx, region, req.SSHKeys)
	if err != nil {
		return provider.Server{}, err
	}

	build := func(diskCategory string, dryRun bool) map[string]string {
		params := map[string]string{
			"RegionId": region, "ZoneId": zone, "ImageId": imageID, "InstanceType": req.Size,
			"SecurityGroupId": sgID, "VSwitchId": vswID, "InstanceChargeType": postpaid,
			"InternetChargeType": "PayByTraffic", "InternetMaxBandwidthOut": defaultBandwidthMbps,
			"SystemDisk.Category": diskCategory, "SystemDisk.Size": strconv.Itoa(defaultSystemDiskGB),
			"InstanceName": req.Name,
		}
		if len(keys) > 0 {
			params["KeyPairName"] = keys[0] // 阿里云一台实例只绑一个密钥对
		}
		if req.UserData != "" {
			params["UserData"] = base64.StdEncoding.EncodeToString([]byte(req.UserData))
		}
		for i, t := range tagPairs(req.Tags, maxTagsPerCreate) {
			params[fmt.Sprintf("Tag.%d.Key", i+1)] = t.key
			params[fmt.Sprintf("Tag.%d.Value", i+1)] = t.value
		}
		if host := hostName(req.Name); host != "" {
			params["HostName"] = host
		}
		if dryRun {
			params["DryRun"] = "true"
		} else {
			// 幂等令牌：客户端遇 5xx 重试时同一个令牌不会重复开机
			params["ClientToken"] = clientToken(req.Name)
		}
		return params
	}

	category, err := c.pickDiskCategory(ctx, region, build)
	if err != nil {
		return provider.Server{}, err
	}
	var out struct {
		InstanceIdSets struct {
			InstanceIdSet []string `json:"InstanceIdSet"`
		} `json:"InstanceIdSets"`
	}
	if err := c.call(ctx, region, "RunInstances", build(category, false), &out); err != nil {
		return provider.Server{}, err
	}
	ids := out.InstanceIdSets.InstanceIdSet
	if len(ids) != 1 {
		return provider.Server{}, fmt.Errorf("RunInstances 返回 %d 个实例 ID，期望 1 个: %v", len(ids), ids)
	}
	c.index.Put(ids[0], region)
	return c.awaitVisible(ctx, region, ids[0])
}

// pickDiskCategory 用 DryRun 选出该规格在该可用区支持的系统盘类型。DryRun 通过时
// 阿里云返回的是错误码 DryRunOperation（而不是空响应）。每种类型都用 DryRun 发
// 一遍完整建机参数，所以它同时是建机前的整体预检：参数、库存、余额的问题在这里
// 就报出来，不会先扣费再失败。
func (c *Client) pickDiskCategory(ctx context.Context, region string, build func(diskCategory string, dryRun bool) map[string]string) (string, error) {
	var lastErr error
	for _, category := range diskCandidates {
		err := c.call(ctx, region, "RunInstances", build(category, true), nil)
		if err == nil || aliyun.IsCode(err, "DryRunOperation") {
			return category, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("建机预检（DryRun）未通过，已试云盘类型 %v: %w", diskCandidates, lastErr)
}

// awaitVisible 等刚建好的实例能查到（RunInstances 返回时实例还在落库，立即可查
// 并不保证）。
func (c *Client) awaitVisible(ctx context.Context, region, id string) (provider.Server, error) {
	deadline := time.Now().Add(createVisibleWait)
	for {
		s, err := c.Get(ctx, id)
		if err == nil {
			return s, nil
		}
		if ctx.Err() != nil {
			return provider.Server{}, ctx.Err()
		}
		if time.Now().After(deadline) {
			return provider.Server{}, fmt.Errorf("实例 %s 已创建（地域 %s）但 %v 内查不到: %w", id, region, createVisibleWait, err)
		}
		if err := c.sleep(ctx, createVisibleInterval); err != nil {
			return provider.Server{}, err
		}
	}
}

// defaultNetwork 在地域里选交换机与安全组：给了可用区就取该可用区的交换机，否则
// 全地域取（默认交换机优先，其次可用 IP 最多的）；安全组取交换机所在 VPC 里 ID
// 最小的一个。返回的可用区即交换机所在区。
func (c *Client) defaultNetwork(ctx context.Context, region, zoneHint string) (vswID, sgID, zone string, err error) {
	params := map[string]string{"RegionId": region, "PageSize": "50"}
	if zoneHint != "" {
		params["ZoneId"] = zoneHint
	}
	var switches struct {
		VSwitches struct {
			VSwitch []vswitch `json:"VSwitch"`
		} `json:"VSwitches"`
	}
	if err = c.call(ctx, region, "DescribeVSwitches", params, &switches); err != nil {
		return "", "", "", fmt.Errorf("查询地域 %s 的交换机失败: %w", region, err)
	}
	if len(switches.VSwitches.VSwitch) == 0 {
		return "", "", "", fmt.Errorf("地域 %s %s没有可用的交换机：请先在阿里云控制台该地域创建 VPC 与交换机（ECS 控制台 → 网络与安全 → 专有网络），或换一个已建好网络的地域",
			region, zoneHintText(zoneHint))
	}
	best := switches.VSwitches.VSwitch[0]
	for _, v := range switches.VSwitches.VSwitch[1:] {
		if betterSwitch(v, best) {
			best = v
		}
	}
	var groups struct {
		SecurityGroups struct {
			SecurityGroup []securityGroup `json:"SecurityGroup"`
		} `json:"SecurityGroups"`
	}
	err = c.call(ctx, region, "DescribeSecurityGroups", map[string]string{
		"RegionId": region, "VpcId": best.VpcId, "PageSize": "50",
	}, &groups)
	if err != nil {
		return "", "", "", fmt.Errorf("查询地域 %s 交换机 %s 所在 VPC 的安全组失败: %w", region, best.VSwitchId, err)
	}
	sgs := groups.SecurityGroups.SecurityGroup
	if len(sgs) == 0 {
		return "", "", "", fmt.Errorf("地域 %s 的 VPC %s 下没有安全组：请先在阿里云控制台为该 VPC 创建安全组（需放行 SSH 端口）", region, best.VpcId)
	}
	sort.Slice(sgs, func(i, j int) bool { return sgs[i].SecurityGroupId < sgs[j].SecurityGroupId })
	return best.VSwitchId, sgs[0].SecurityGroupId, best.ZoneId, nil
}

func zoneHintText(zoneHint string) string {
	if zoneHint == "" {
		return ""
	}
	return "的可用区 " + zoneHint + " "
}

// betterSwitch 给交换机排序：默认交换机优先，其次可用 IP 多的，再按 ID 保证结果稳定。
func betterSwitch(a, b vswitch) bool {
	if a.IsDefault != b.IsDefault {
		return a.IsDefault
	}
	if a.AvailableIpAddressCount != b.AvailableIpAddressCount {
		return a.AvailableIpAddressCount > b.AvailableIpAddressCount
	}
	return a.VSwitchId < b.VSwitchId
}

type tagPair struct{ key, value string }

// tagPairs 把 vpsctl 的扁平标签拆成键值对：按第一个冒号切，没冒号的整串作键、值
// 留空。阿里云标签键不能带"acs:"/"aliyun"前缀，带了的原样交给平台报错。
func tagPairs(tags []string, max int) []tagPair {
	out := make([]tagPair, 0, min(len(tags), max))
	for _, t := range tags {
		if len(out) == max {
			break
		}
		key, value, _ := strings.Cut(t, ":")
		if key == "" {
			continue
		}
		out = append(out, tagPair{key, value})
	}
	return out
}

// hostName 把节点名转成阿里云接受的 OS 主机名；不合规则的返回空串（不设，
// 由平台随机起名）。规则：2-60 位字母数字，点和连字符不能开头、结尾或连续。
var hostNameRe = regexp.MustCompile(`^[A-Za-z0-9]+(?:[.-][A-Za-z0-9]+)*$`)

func hostName(name string) string {
	if len(name) < 2 || len(name) > 60 || !hostNameRe.MatchString(name) {
		return ""
	}
	return name
}

// clientToken 生成幂等令牌：节点名片段 + 随机串，≤64 个 ASCII 字符。
func clientToken(name string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	prefix := make([]byte, 0, 32)
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			prefix = append(prefix, byte(r))
		}
		if len(prefix) == 32 {
			break
		}
	}
	return "vpsctl-" + string(prefix) + "-" + hex.EncodeToString(b[:])
}
