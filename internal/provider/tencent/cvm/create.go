package cvm

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
)

// create.go：建机。按量计费（与 DO 的按小时同语义），落在该地域的默认 VPC 与
// 默认子网上，安全组不指定——腾讯云会绑定项目默认安全组，没有则自动创建。

const (
	// defaultSystemDiskGB / defaultBandwidthMbps 是建机缺省的系统盘大小与公网带宽上限。
	// 公网按流量计费，带宽上限本身不产生费用。
	defaultSystemDiskGB   = 50
	defaultBandwidthMbps  = 10
	createVisibleWait     = 30 * time.Second
	createVisibleInterval = time.Second
)

// diskCandidates 是系统盘云盘类型的尝试顺序（由省到贵）。各机型在各可用区支持的
// 云盘类型不一，又没有稳定的查询接口，建机前用 DryRun 逐个试，取第一个通过的。
var diskCandidates = []string{"CLOUD_BSSD", "CLOUD_PREMIUM", "CLOUD_HSSD", "CLOUD_SSD"}

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
	vpcID, subnetID, zone, err := c.defaultNetwork(ctx, region, zoneHint)
	if err != nil {
		return provider.Server{}, err
	}
	keys, err := c.resolveSSHKeys(ctx, region, req.SSHKeys)
	if err != nil {
		return provider.Server{}, err
	}

	build := func(diskType string, dryRun bool) map[string]any {
		params := map[string]any{
			"InstanceChargeType": postpaid,
			"Placement":          map[string]any{"Zone": zone},
			"InstanceType":       req.Size,
			"ImageId":            imageID,
			"VirtualPrivateCloud": map[string]any{
				"VpcId": vpcID, "SubnetId": subnetID,
			},
			"InternetAccessible": map[string]any{
				"InternetChargeType":      "TRAFFIC_POSTPAID_BY_HOUR",
				"InternetMaxBandwidthOut": defaultBandwidthMbps,
				"PublicIpAssigned":        true,
			},
			"SystemDisk":      map[string]any{"DiskType": diskType, "DiskSize": defaultSystemDiskGB},
			"InstanceName":    req.Name,
			"EnhancedService": map[string]any{"MonitorService": map[string]any{"Enabled": req.Monitoring}},
		}
		if len(keys) > 0 {
			params["LoginSettings"] = map[string]any{"KeyIds": keys}
		}
		if req.UserData != "" {
			params["UserData"] = base64.StdEncoding.EncodeToString([]byte(req.UserData))
		}
		if len(req.Tags) > 0 {
			params["TagSpecification"] = []map[string]any{{
				"ResourceType": "instance", "Tags": tencentTags(req.Tags),
			}}
		}
		if host := hostName(req.Name); host != "" {
			params["HostName"] = host
		}
		if dryRun {
			params["DryRun"] = true
		} else {
			// 幂等令牌：客户端遇 5xx 重试时同一个令牌不会重复开机
			params["ClientToken"] = clientToken(req.Name)
		}
		return params
	}

	diskType, err := c.pickDiskType(ctx, region, build)
	if err != nil {
		return provider.Server{}, err
	}
	var out struct {
		InstanceIdSet []string `json:"InstanceIdSet"`
	}
	if err := c.api.Call(ctx, host, service, version, "RunInstances", region, build(diskType, false), &out); err != nil {
		return provider.Server{}, err
	}
	if len(out.InstanceIdSet) != 1 {
		return provider.Server{}, fmt.Errorf("RunInstances 返回 %d 个实例 ID，期望 1 个: %v", len(out.InstanceIdSet), out.InstanceIdSet)
	}
	id := out.InstanceIdSet[0]
	c.index.Put(id, region)
	return c.awaitVisible(ctx, region, id)
}

// pickDiskType 用 DryRun 选出该机型在该可用区支持的系统盘类型。每种类型都用
// DryRun 发一遍完整建机参数，所以它同时是建机前的整体预检：参数、库存、余额
// 的问题在这里就报出来，不会先扣费再失败。
func (c *Client) pickDiskType(ctx context.Context, region string, build func(diskType string, dryRun bool) map[string]any) (string, error) {
	var lastErr error
	for _, diskType := range diskCandidates {
		err := c.api.Call(ctx, host, service, version, "RunInstances", region, build(diskType, true), nil)
		if err == nil {
			return diskType, nil
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
		inst, err := c.describe(ctx, region, id)
		if err == nil {
			s := c.toServer(region, inst)
			s.Image = c.imageSlugByID(ctx, region, inst.ImageId)
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

// defaultNetwork 取地域的默认 VPC，并在其中选一个子网：给了可用区就取该可用区的
// 子网，否则取默认子网（没有默认子网则取可用 IP 最多的）。返回的可用区即子网所在区。
func (c *Client) defaultNetwork(ctx context.Context, region, zoneHint string) (vpcID, subnetID, zone string, err error) {
	vpcID, err = c.defaultVpc(ctx, region)
	if err != nil {
		return "", "", "", err
	}
	var subnets struct {
		SubnetSet []subnet `json:"SubnetSet"`
	}
	err = c.api.Call(ctx, vpcHost, vpcService, version, "DescribeSubnets", region, map[string]any{
		"Limit":   fmt.Sprint(pageSize),
		"Filters": []map[string]any{{"Name": "vpc-id", "Values": []string{vpcID}}},
	}, &subnets)
	if err != nil {
		return "", "", "", fmt.Errorf("查询地域 %s 默认 VPC %s 的子网失败: %w", region, vpcID, err)
	}
	var zones []string
	best := -1
	for i, s := range subnets.SubnetSet {
		if s.VpcId != vpcID {
			continue
		}
		zones = append(zones, s.Zone)
		if zoneHint != "" && s.Zone != zoneHint {
			continue
		}
		if best < 0 || betterSubnet(s, subnets.SubnetSet[best]) {
			best = i
		}
	}
	if best < 0 {
		if len(zones) == 0 {
			return "", "", "", fmt.Errorf("地域 %s 的默认 VPC %s 下没有子网：请在控制台为该 VPC 建一个子网后再建机", region, vpcID)
		}
		return "", "", "", fmt.Errorf("地域 %s 的可用区 %s 在默认 VPC 下没有子网（有子网的可用区: %s）；请换一个可用区，或先在控制台为该可用区建子网",
			region, zoneHint, strings.Join(zones, ", "))
	}
	s := subnets.SubnetSet[best]
	return vpcID, s.SubnetId, s.Zone, nil
}

// betterSubnet 给同 VPC 下的子网排序：默认子网优先，其次可用 IP 多的，再按子网 ID
// 保证结果稳定。
func betterSubnet(a, b subnet) bool {
	if a.IsDefault != b.IsDefault {
		return a.IsDefault
	}
	if a.AvailableIpAddressCount != b.AvailableIpAddressCount {
		return a.AvailableIpAddressCount > b.AvailableIpAddressCount
	}
	return a.SubnetId < b.SubnetId
}

// defaultVpc 取地域的默认 VPC。没有默认 VPC 的地域 vpsctl 不代建网络——建 VPC
// 属于账号级的网络规划，报错让用户在控制台处理。
func (c *Client) defaultVpc(ctx context.Context, region string) (string, error) {
	var vpcs struct {
		VpcSet []vpc `json:"VpcSet"`
	}
	err := c.api.Call(ctx, vpcHost, vpcService, version, "DescribeVpcs", region, map[string]any{"Limit": fmt.Sprint(pageSize)}, &vpcs)
	if err != nil {
		return "", fmt.Errorf("查询地域 %s 的 VPC 失败: %w", region, err)
	}
	for _, v := range vpcs.VpcSet {
		if v.IsDefault {
			return v.VpcId, nil
		}
	}
	return "", fmt.Errorf("地域 %s 没有默认 VPC：请先在腾讯云控制台该地域创建默认 VPC（VPC 控制台 → 默认 VPC），或换一个已有默认 VPC 的地域", region)
}

// hostName 把节点名转成腾讯云接受的 OS 主机名；不合规则的返回空串（不设，
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
