// Package provider 定义跨 VPS 提供商的统一接口与注册表。
// 各提供商实现放子包（digitalocean/，未来 vultr/、aliyun/），
// 通过 init + Register 接入；上层编排与 UI 只依赖本包。
package provider

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"
)

// 电源动作类型（各提供商语义对齐）。
const (
	PowerOff = "power_off" // 硬断电，立即生效
	PowerOn  = "power_on"
	Shutdown = "shutdown" // 优雅关机，依赖 guest 响应，可能超时
)

// Server 是提供商侧一台虚拟机的统一视图。
type Server struct {
	ID           string  // 提供商内唯一 ID（DO 为十进制数字串）
	Account      string  // 所属配置账号名
	Name         string  //
	Status       string  // active / new / off ...
	Region       string  // slug
	Size         string  // 套餐 slug
	Image        string  // 镜像 slug（自定义镜像可能为空）
	VCPUs        int     //
	MemoryMB     int     //
	DiskGB       int     //
	PriceMonthly float64 // USD
	IPv4Public   string  //
	IPv4Private  string  //
	Tags         []string
	CreatedAt    time.Time
}

// CreateRequest 描述一次创建请求。
type CreateRequest struct {
	Name       string
	Region     string
	Size       string
	Image      string
	SSHKeys    []string // ID / 指纹 / 名称混填，由实现自行解析
	Tags       []string
	UserData   string // cloud-init 脚本，可空
	Monitoring bool
}

// ActionRef 指向一次异步操作的句柄，用于轮询。
type ActionRef struct {
	ID string
}

// SSHKey 是账号下的公钥。
type SSHKey struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

// Region 是可部署区域。
type Region struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Size 是套餐规格。
type Size struct {
	Slug         string  `json:"slug"`
	VCPUs        int     `json:"vcpus"`
	MemoryMB     int     `json:"memory_mb"`
	DiskGB       int     `json:"disk_gb"`
	PriceMonthly float64 `json:"price_monthly"`
}

// Image 是系统镜像。
type Image struct {
	Slug         string `json:"slug"`
	Distribution string `json:"distribution"`
	Name         string `json:"name"`
}

// Provider 是单个账号视角下的提供商客户端，实现需并发安全。
type Provider interface {
	List(ctx context.Context) ([]Server, error)
	Get(ctx context.Context, id string) (Server, error)
	Create(ctx context.Context, req CreateRequest) (Server, error)
	Delete(ctx context.Context, id string) error
	Power(ctx context.Context, id string, action string) (ActionRef, error)
	// ActionStatus 返回 in-progress / completed / errored。
	ActionStatus(ctx context.Context, ref ActionRef) (string, error)
	SSHKeys(ctx context.Context) ([]SSHKey, error)
	Regions(ctx context.Context) ([]Region, error)
	Sizes(ctx context.Context) ([]Size, error)
	Images(ctx context.Context) ([]Image, error)
}

// Factory 按 (账号名, token) 构造客户端；hc 为 nil 时用默认 http.Client。
type Factory func(account, token string, hc *http.Client) (Provider, error)

var registry = map[string]Factory{}

// Register 注册提供商实现，供各实现包的 init 调用。
func Register(name string, f Factory) {
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("provider: %q 重复注册", name))
	}
	registry[name] = f
}

// New 按提供商名构造对应客户端。
func New(name, account, token string, hc *http.Client) (Provider, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("provider: 未知提供商 %q（已注册: %v）", name, Names())
	}
	return f(account, token, hc)
}

// Names 返回已注册的提供商名（升序）。
func Names() []string {
	return slices.Sorted(maps.Keys(registry))
}
