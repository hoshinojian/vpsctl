package nms

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hoshinojian/vpsctl/internal/fleet"
)

func server(account, name, ip string) fleet.ServerJSON {
	created := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	return fleet.ServerJSON{
		ID: "3164444", Account: account, Name: name, Status: "active",
		Region: "sgp1", Size: "s-1vcpu-1gb", Image: "ubuntu-24-04-x64",
		VCPUs: 1, MemoryMB: 1024, DiskGB: 25, PriceMonthly: 6,
		IPv4Public: ip, Tags: []string{"batch:20260908T120000Z"}, CreatedAt: created,
	}
}

// accounts 构造账号侧信息映射（provider 名 + SSH 凭据）。
func accounts(entries map[string]Account) map[string]Account { return entries }

func TestNodesIDIsName(t *testing.T) {
	got := Nodes([]fleet.ServerJSON{server("do-1", "vps-do-1-01", "203.0.113.10")}, nil)
	if len(got) != 1 {
		t.Fatalf("节点数 = %d, want 1", len(got))
	}
	n := got[0]
	if n.ID != "vps-do-1-01" {
		t.Errorf("id = %q, want 节点名 vps-do-1-01（重建同名走 NMS 复活通道）", n.ID)
	}
	if n.Name != "vps-do-1-01" {
		t.Errorf("name = %q", n.Name)
	}
}

func TestNodesDefaults(t *testing.T) {
	got := Nodes([]fleet.ServerJSON{server("do-1", "vps-do-1-01", "203.0.113.10")}, nil)
	n := got[0]
	if n.DeviceType != "vps" {
		t.Errorf("device_type = %q, want vps", n.DeviceType)
	}
	if n.SSHPort != 22 {
		t.Errorf("ssh_port = %d, want 22", n.SSHPort)
	}
	if n.SSHUser != "root" {
		t.Errorf("ssh_user = %q, want root（凭据缺省）", n.SSHUser)
	}
	if n.ManagementIP != "203.0.113.10" {
		t.Errorf("management_ip = %q", n.ManagementIP)
	}
	if n.Region != "sgp1" {
		t.Errorf("region = %q", n.Region)
	}
}

func TestNodesCredentialFromAccount(t *testing.T) {
	creds := accounts(map[string]Account{
		"do-1": {Provider: "digitalocean", SSHUser: "deploy", SSHPassword: "s3cret"},
	})
	got := Nodes([]fleet.ServerJSON{server("do-1", "vps-do-1-01", "203.0.113.10")}, creds)
	n := got[0]
	if n.SSHUser != "deploy" || n.SSHPassword != "s3cret" {
		t.Errorf("凭据未注入: user=%q password=%q", n.SSHUser, n.SSHPassword)
	}
	// 未配置凭据的账号落回缺省，密码留空（由调用方决定是否告警）
	got = Nodes([]fleet.ServerJSON{server("do-2", "vps-do-2-01", "203.0.113.11")}, creds)
	if got[0].SSHUser != "root" || got[0].SSHPassword != "" {
		t.Errorf("无凭据账号应落缺省: %+v", got[0])
	}
}

func TestNodesCarryHardwareAndProvider(t *testing.T) {
	creds := accounts(map[string]Account{"do-1": {Provider: "digitalocean"}})
	got := Nodes([]fleet.ServerJSON{server("do-1", "vps-do-1-01", "203.0.113.10")}, creds)
	n := got[0]
	if n.Provider != "digitalocean" {
		t.Errorf("provider = %q", n.Provider)
	}
	if n.RAMMB != 1024 || n.DiskGB != 25 || n.CPUCores != 1 || n.CostMonthly != 6 {
		t.Errorf("硬件/成本字段不符: %+v", n)
	}
	if !strings.HasPrefix(n.ProvisionedAt, "2026-09-08T12:00:00") {
		t.Errorf("provisioned_at = %q, want RFC3339", n.ProvisionedAt)
	}
}

func TestNodesSortedByAccountThenName(t *testing.T) {
	servers := []fleet.ServerJSON{
		server("do-2", "vps-do-2-01", "203.0.113.11"),
		server("do-1", "vps-do-1-02", "203.0.113.10"),
		server("do-1", "vps-do-1-01", "203.0.113.9"),
	}
	got := Nodes(servers, nil)
	want := []string{"vps-do-1-01", "vps-do-1-02", "vps-do-2-01"}
	for i, n := range got {
		if n.Name != want[i] {
			t.Fatalf("排序不符[%d] = %s, want %s", i, n.Name, want[i])
		}
	}
}

func TestPayloadJSONShape(t *testing.T) {
	servers := []fleet.ServerJSON{server("do-1", "vps-do-1-01", "203.0.113.10")}
	creds := accounts(map[string]Account{"do-1": {Provider: "digitalocean", SSHPassword: "pw"}})
	b, err := json.Marshal(Payload{Nodes: Nodes(servers, creds)})
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatal(err)
	}
	nodes, ok := probe["nodes"].([]any)
	if !ok || len(nodes) != 1 {
		t.Fatalf("载荷应为 {\"nodes\":[…]}: %s", b)
	}
	n := nodes[0].(map[string]any)
	for _, k := range []string{"id", "name", "device_type", "management_ip",
		"ssh_port", "ssh_user", "ssh_password", "region", "provider"} {
		if _, ok := n[k]; !ok {
			t.Errorf("缺字段 %q: %s", k, b)
		}
	}
}
