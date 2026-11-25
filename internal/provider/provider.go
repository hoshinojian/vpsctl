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
	PriceMonthly float64 // 每月单价，币种见 Currency
	// Currency 是 PriceMonthly 的币种（ISO 4217，如 USD / CNY）；空视为 USD。
	// 管理台按币种分别合计，不同币种不能相加。
	Currency    string //
	IPv4Public  string //
	IPv4Private string //
	Tags        []string
	CreatedAt   time.Time
	// ExpiresAt 是包年包月节点的到期时间（按量节点为零值，不出现在 JSON 里）。
	ExpiresAt time.Time
	// Lat/Lng 是节点所在地理坐标（等距投影用）。提供商 API 通常不直接返回，
	// 由各实现在 toServer 里按 region slug 从静态映射表填写；未知 slug 为 0。
	Lat float64
	Lng float64
}

// Login 是给节点注入的登录凭据（vpsctl 建机时生成的随机 root 密码）。
type Login struct {
	User     string // 缺省 root
	Password string
}

// CreateRequest 描述一次创建请求。
type CreateRequest struct {
	Name       string
	Region     string
	Size       string
	Image      string
	SSHKeys    []string // ID / 指纹 / 名称混填，由实现自行解析
	Tags       []string
	UserData   string // cloud-init 脚本，可空；与 Login 是同一份凭据的两种载体
	Monitoring bool
	// Login 是 UserData 里那份密码的结构化形式：走 cloud-init 的实现用 UserData，没有
	// 用户数据通道、只能靠平台接口设密码的实现（轻量应用服务器）用 Login。都不给就是
	// 用户自己用 --user-data 置备。
	Login Login
	// 以下三项只经 cloud-init 落地；没有 cloud-init 的实现遇到非零值要明确拒绝。
	SSHPort        int // 建机注入的 sshd 监听端口（0/22 = 缺省）
	Tunnel443      bool
	AuthorizedKeys []string
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
	Currency     string  `json:"currency,omitempty"` // 空视为 USD
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
	// Rebuild 用指定镜像重装节点：保留 ID/IP，磁盘数据清空、不可恢复。
	Rebuild(ctx context.Context, id, image string) (ActionRef, error)
	// Resize 变更套餐规格；节点须处于关机状态（编排层负责开关机链）。
	// resizeDisk=true 同时扩磁盘（磁盘不可缩，属不可逆）。
	Resize(ctx context.Context, id, size string, resizeDisk bool) (ActionRef, error)
	SSHKeys(ctx context.Context) ([]SSHKey, error)
	Regions(ctx context.Context) ([]Region, error)
	Sizes(ctx context.Context) ([]Size, error)
	Images(ctx context.Context) ([]Image, error)
}

// AuthKind 是提供商的凭据形态，供配置与管理台决定该让用户填什么。
type AuthKind string

const (
	AuthToken   AuthKind = "token"   // 单个 API Token（DigitalOcean）
	AuthKeyPair AuthKind = "keypair" // AccessKey ID + Secret（腾讯云、阿里云）
)

// Credentials 是账号的提供商凭据：单 Token 型提供商只看 Token，密钥对型只看
// AccessKey + SecretKey。由 config.Account.Credentials 生成。
type Credentials struct {
	Token     string
	AccessKey string // 腾讯云 SecretId / 阿里云 AccessKey ID
	SecretKey string // 腾讯云 SecretKey / 阿里云 AccessKey Secret
}

// Prechecker 是可选能力：建机前让提供商做一次不落地的预检（云厂商的 DryRun）。参数、
// 库存、余额这类建机才会暴露的问题在这里就报出来，对建一次就付一个月的产品尤其要紧。
type Prechecker interface {
	Precheck(ctx context.Context, req CreateRequest) error
}

// Rebuilder 是可选能力：重装时必须重新下发登录凭据的实现。
//
// 没有 cloud-init 用户数据通道、只能靠平台接口设密码的提供商（腾讯云轻量应用服务器）
// 重装时系统盘整个重建，平台密码不会随之保留：不重新下发的话，节点建得出来却登不进去，
// 而密码库里仍留着旧密码——导出 NMS 载荷会给出一个机器上根本不存在的凭据。所以这类
// 提供商必须走 RebuildWithLogin，由编排层先把该节点的凭据查出来传进来。
//
// 没有凭据记录（例如 --no-password 建的节点）一律拒绝重装，不能当普通重装放过去。
type Rebuilder interface {
	RebuildWithLogin(ctx context.Context, id, image string, login Login) (ActionRef, error)
}

// Factory 按账号名与凭据构造客户端；hc 为 nil 时用默认 http.Client。
type Factory func(account string, cred Credentials, hc *http.Client) (Provider, error)

var registry = map[string]Factory{}
var authKinds = map[string]AuthKind{}

// Register 注册提供商实现及其凭据形态，供各实现包的 init 调用。
func Register(name string, auth AuthKind, f Factory) {
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("provider: %q 重复注册", name))
	}
	registry[name] = f
	authKinds[name] = auth
}

// New 按提供商名构造对应客户端。
func New(name, account string, cred Credentials, hc *http.Client) (Provider, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("provider: 未知提供商 %q（已注册: %v）", name, Names())
	}
	return f(account, cred, hc)
}

// Names 返回已注册的提供商名（升序）。
func Names() []string {
	return slices.Sorted(maps.Keys(registry))
}

var chpasswdCompat = map[string]bool{}

// RegisterChpasswdCompat 标记提供商的公共镜像带旧版 / 定制版 cloud-init：注入密码的
// cloud-init 要同时下发老的 chpasswd.list 写法（见 fleet.CloudInitParams.ChpasswdCompat）。
// 由对应 provider 的 init 调用。
func RegisterChpasswdCompat(name string) { chpasswdCompat[name] = true }

// ChpasswdCompat 报告某提供商是否需要兼容老的 cloud-init 密码写法；未注册的提供商名按 false。
func ChpasswdCompat(name string) bool { return chpasswdCompat[name] }

// KeyPairProviders 返回凭据为密钥对型（AccessKey + Secret）的提供商名（升序）。
// 未注册的提供商名不在返回值里。
func KeyPairProviders() []string {
	var out []string
	for name, kind := range authKinds {
		if kind == AuthKeyPair {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
