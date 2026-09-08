// Package nms 把 vpsctl 节点渲染成 NMS（nms 仓）节点台账的导入载荷，
// 字段对齐 NMS 04 §1.1（POST /api/v1/topology 的 ImportNode，upsert 幂等）。
// id 取节点名：重建后同名重导正好走 NMS 的重录复活通道；
// 硬件/成本字段 NMS 现载荷不收（解码忽略未知字段），冗余输出待其扩契约后直接可用。
package nms

import (
	"sort"
	"time"

	"github.com/hoshinojian/vpsctl/internal/fleet"
)

// Account 是渲染载荷所需的账号侧信息（提供商名 + 导入用的 SSH 凭据）。
type Account struct {
	Provider    string
	SSHUser     string
	SSHPassword string
}

// Node 是载荷里的单个节点（JSON 字段名对齐 NMS ImportNode）。
type Node struct {
	ID           string `json:"id"` // = Name，理由见包注释
	Name         string `json:"name"`
	DeviceType   string `json:"device_type"` // 恒 vps
	ManagementIP string `json:"management_ip"`
	SSHPort      int    `json:"ssh_port"` // DO 固定 22
	SSHUser      string `json:"ssh_user"`
	SSHPassword  string `json:"ssh_password"`
	Region       string `json:"region,omitempty"`
	// 以下为 NMS 现载荷不收的冗余字段（json.Decode 忽略未知字段，无害）
	Provider      string  `json:"provider,omitempty"`
	RAMMB         int     `json:"ram_mb,omitempty"`
	DiskGB        int     `json:"disk_gb,omitempty"`
	CPUCores      int     `json:"cpu_cores,omitempty"`
	CostMonthly   float64 `json:"cost_monthly,omitempty"`
	ProvisionedAt string  `json:"provisioned_at,omitempty"` // RFC3339
}

// Payload 是 POST /api/v1/topology 的请求体形态。
type Payload struct {
	Nodes []Node `json:"nodes"`
}

// Nodes 把跨账号节点渲染为载荷节点列表，按账号名、节点名排序。
// accounts 缺某账号时凭据落缺省（root / 空密码），由调用方决定是否告警。
func Nodes(servers []fleet.ServerJSON, accounts map[string]Account) []Node {
	sorted := append([]fleet.ServerJSON(nil), servers...)
	sort.Slice(sorted, func(a, b int) bool {
		if sorted[a].Account != sorted[b].Account {
			return sorted[a].Account < sorted[b].Account
		}
		return sorted[a].Name < sorted[b].Name
	})
	out := make([]Node, 0, len(sorted))
	for _, s := range sorted {
		var ac Account
		if accounts != nil {
			ac = accounts[s.Account]
		}
		user := ac.SSHUser
		if user == "" {
			user = "root"
		}
		out = append(out, Node{
			ID:            s.Name,
			Name:          s.Name,
			DeviceType:    "vps",
			ManagementIP:  s.IPv4Public,
			SSHPort:       22,
			SSHUser:       user,
			SSHPassword:   ac.SSHPassword,
			Region:        s.Region,
			Provider:      ac.Provider,
			RAMMB:         s.MemoryMB,
			DiskGB:        s.DiskGB,
			CPUCores:      s.VCPUs,
			CostMonthly:   s.PriceMonthly,
			ProvisionedAt: s.CreatedAt.Format(time.RFC3339),
		})
	}
	return out
}
