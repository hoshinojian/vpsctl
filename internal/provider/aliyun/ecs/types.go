// Package ecs 实现 provider.Provider 的阿里云云服务器（ECS，按量付费）适配。
//
// 与 DigitalOcean 的关键差异都由本包吸收，上层编排无感：
//   - 地域型 API：查实例必须带地域，且要打地域专属接入点（ecs.<地域>.aliyuncs.com；
//     33 个地域里有 14 个较新的地域中心接入点不服务）。List 逐地域并发；按 ID
//     操作先查进程内的 ID→地域索引（List / Create 时登记），未命中则逐地域找一次。
//   - 套餐就是规格 ID（如 ecs.u1-c1m1.large）；镜像与密钥对按地域区分。镜像 slug
//     跨地域稳定（如 ubuntu-24.04），建机 / 重装时在该地域解析成 ImageId。
//   - 异步接口不返回任务号：Power / Rebuild 返回的句柄里记着"等什么状态"，
//     ActionStatus 靠回读实例状态判定；改配是同步接口，看规格是否已变。
//   - 实例列表不带标签与系统盘：标签走 ListTagResources（按地域批量），系统盘走
//     DescribeDisks（逐台），都带缓存。
//   - 人民币计费：Server / Size 的 Currency 为 CNY，月费 = 按量小时单价 × 730。
//
// 文档: https://help.aliyun.com/zh/ecs/developer-reference/api-overview
package ecs

// 阿里云 ECS OpenAPI 结构（仅本工具用到的字段；响应字段是 PascalCase）。

// instance 是 DescribeInstances 的 Instances.Instance 元素。
type instance struct {
	InstanceId         string `json:"InstanceId"`
	InstanceName       string `json:"InstanceName"`
	InstanceType       string `json:"InstanceType"`
	Status             string `json:"Status"` // Pending / Running / Stopping / Stopped / Starting …
	ZoneId             string `json:"ZoneId"`
	RegionId           string `json:"RegionId"`
	InstanceChargeType string `json:"InstanceChargeType"` // PrePaid / PostPaid
	Cpu                int    `json:"Cpu"`
	Memory             int    `json:"Memory"` // MB
	CreationTime       string `json:"CreationTime"`
	ImageId            string `json:"ImageId"`
	PublicIpAddress    struct {
		IpAddress []string `json:"IpAddress"`
	} `json:"PublicIpAddress"`
	EipAddress struct {
		IpAddress string `json:"IpAddress"`
	} `json:"EipAddress"`
	VpcAttributes struct {
		VpcId            string `json:"VpcId"`
		PrivateIpAddress struct {
			IpAddress []string `json:"IpAddress"`
		} `json:"PrivateIpAddress"`
	} `json:"VpcAttributes"`
	// OperationLocks 非空表示实例上有未完成的操作（如更换系统盘进行中）。
	OperationLocks struct {
		LockReason []struct {
			LockReason string `json:"LockReason"`
		} `json:"LockReason"`
	} `json:"OperationLocks"`
}

type instancesPage struct {
	TotalCount int `json:"TotalCount"`
	Instances  struct {
		Instance []instance `json:"Instance"`
	} `json:"Instances"`
}

type image struct {
	ImageId      string `json:"ImageId"`
	ImageName    string `json:"ImageName"`
	OSNameEn     string `json:"OSNameEn"`
	Platform     string `json:"Platform"`
	Architecture string `json:"Architecture"`
	Status       string `json:"Status"`
	Usage        string `json:"Usage"` // instance 表示可用于创建实例
	CreationTime string `json:"CreationTime"`
}

type instanceType struct {
	InstanceTypeId     string  `json:"InstanceTypeId"`
	InstanceTypeFamily string  `json:"InstanceTypeFamily"`
	CpuCoreCount       int     `json:"CpuCoreCount"`
	MemorySize         float64 `json:"MemorySize"` // GiB
	GPUAmount          int     `json:"GPUAmount"`
}

// vswitch / securityGroup 是建机所需的网络元素。
type vswitch struct {
	VSwitchId               string `json:"VSwitchId"`
	VpcId                   string `json:"VpcId"`
	ZoneId                  string `json:"ZoneId"`
	IsDefault               bool   `json:"IsDefault"`
	AvailableIpAddressCount int    `json:"AvailableIpAddressCount"`
}

type securityGroup struct {
	SecurityGroupId string `json:"SecurityGroupId"`
	VpcId           string `json:"VpcId"`
}

// disk 是 DescribeDisks 的 Disks.Disk 元素；只关心系统盘的 ID、类型与容量。
type disk struct {
	DiskId   string `json:"DiskId"`
	Type     string `json:"Type"` // system / data
	Category string `json:"Category"`
	Size     int    `json:"Size"` // GiB
}

// tagResource 是 ListTagResources 的扁平键值对。
type tagResource struct {
	ResourceId string `json:"ResourceId"`
	TagKey     string `json:"TagKey"`
	TagValue   string `json:"TagValue"`
}

type keyPair struct {
	KeyPairName        string `json:"KeyPairName"`
	KeyPairFingerPrint string `json:"KeyPairFingerPrint"`
}

// priceInfo 是 DescribePrice 的结果；按量付费时 TradePrice 是每小时合计。
type priceInfo struct {
	PriceInfo struct {
		Price struct {
			TradePrice float64 `json:"TradePrice"`
			Currency   string  `json:"Currency"`
		} `json:"Price"`
	} `json:"PriceInfo"`
}
