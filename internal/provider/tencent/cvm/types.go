// Package cvm 实现 provider.Provider 的腾讯云云服务器（CVM，按量计费）适配。
//
// 与 DigitalOcean 的关键差异都由本包吸收，上层编排无感：
//   - 地域型 API：查实例必须带地域。List 逐地域并发；按 ID 操作先查进程内的
//     ID→地域索引（List / Create 时登记），未命中则逐地域并发找一次。
//   - 套餐就是机型名（如 SA5.MEDIUM2）；镜像与密钥对按地域区分。镜像 slug 跨
//     地域稳定（如 ubuntu-24.04），建机 / 重装时在该地域解析成 ImageId。
//   - 异步操作没有任务号：Power / Rebuild / Resize 返回的句柄里记着"做了什么、
//     等什么状态"，ActionStatus 靠回读实例状态与 LatestOperation* 字段判定。
//   - 人民币计费：Server / Size 的 Currency 为 CNY，月费 = 按量小时单价 × 730。
//
// 文档: https://cloud.tencent.com/document/api/213/15753 （数据类型与接口）
package cvm

// 腾讯云 API 3.0 结构（仅本工具用到的字段）。

// tag 是腾讯云的键值标签。
type tag struct {
	Key   string `json:"Key"`
	Value string `json:"Value"`
}

// instance 是 CVM 实例（DescribeInstances 的 InstanceSet 元素）。
type instance struct {
	InstanceId         string   `json:"InstanceId"`
	InstanceName       string   `json:"InstanceName"`
	InstanceType       string   `json:"InstanceType"`
	InstanceState      string   `json:"InstanceState"`
	InstanceChargeType string   `json:"InstanceChargeType"` // POSTPAID_BY_HOUR / PREPAID …
	CPU                int      `json:"CPU"`
	Memory             int      `json:"Memory"` // GB
	CreatedTime        string   `json:"CreatedTime"`
	PublicIpAddresses  []string `json:"PublicIpAddresses"`
	PrivateIpAddresses []string `json:"PrivateIpAddresses"`
	ImageId            string   `json:"ImageId"`
	Placement          struct {
		Zone string `json:"Zone"`
	} `json:"Placement"`
	SystemDisk struct {
		DiskId   string `json:"DiskId"`
		DiskType string `json:"DiskType"`
		DiskSize int    `json:"DiskSize"`
	} `json:"SystemDisk"`
	Tags []tag `json:"Tags"`

	// 最新一次异步操作及其状态（OPERATE_SUCCESS / OPERATE_RUNNING / OPERATE_FAILED …），
	// 是没有任务号的异步接口唯一的进度信号；可能是上一轮操作留下的旧值。
	LatestOperation         string `json:"LatestOperation"`
	LatestOperationState    string `json:"LatestOperationState"`
	LatestOperationErrorMsg string `json:"LatestOperationErrorMsg"`
}

type instancesPage struct {
	TotalCount  int        `json:"TotalCount"`
	InstanceSet []instance `json:"InstanceSet"`
}

type image struct {
	ImageId      string `json:"ImageId"`
	ImageName    string `json:"ImageName"`
	Platform     string `json:"Platform"`
	OsType       string `json:"OsType"` // linux / windows
	Architecture string `json:"Architecture"`
	ImageState   string `json:"ImageState"`
	ImageType    string `json:"ImageType"` // PUBLIC_IMAGE 为公共镜像
}

type keyPair struct {
	KeyPairId string `json:"KeyPairId"`
	KeyName   string `json:"KeyName"`
	PublicKey string `json:"PublicKey"`
}

type instanceTypeConfig struct {
	InstanceType   string `json:"InstanceType"`
	InstanceFamily string `json:"InstanceFamily"`
	CPU            int    `json:"CPU"`
	Memory         int    `json:"Memory"` // GB
	GPU            int    `json:"GPU"`
	FPGA           int    `json:"FPGA"`
}

// vpc / subnet 是默认 VPC 与子网的查询结果元素（VPC 产品的接口）。
type vpc struct {
	VpcId     string `json:"VpcId"`
	IsDefault bool   `json:"IsDefault"`
}

type subnet struct {
	SubnetId                string `json:"SubnetId"`
	VpcId                   string `json:"VpcId"`
	Zone                    string `json:"Zone"`
	IsDefault               bool   `json:"IsDefault"`
	AvailableIpAddressCount int    `json:"AvailableIpAddressCount"`
}

// hourlyPrice 是询价接口里"每台每小时"的那一档价格。
type hourlyPrice struct {
	UnitPrice         float64 `json:"UnitPrice"`
	UnitPriceDiscount float64 `json:"UnitPriceDiscount"`
}
