// Package digitalocean 实现 provider.Provider 的 DigitalOcean 适配。
package digitalocean

// DigitalOcean API v2 结构（仅本工具用到的字段）。
// 文档: https://docs.digitalocean.com/reference/api/api-reference/

type region struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	Available bool   `json:"available"`
}

type image struct {
	Slug         string `json:"slug"`
	Distribution string `json:"distribution"`
	Name         string `json:"name"`
}

type size struct {
	Slug         string  `json:"slug"`
	Memory       int     `json:"memory"` // MB
	VCPUs        int     `json:"vcpus"`
	Disk         int     `json:"disk"` // GB
	PriceMonthly float64 `json:"price_monthly"`
	Available    bool    `json:"available"`
}

type netV4 struct {
	IPAddress string `json:"ip_address"`
	Type      string `json:"type"` // public / private
}

type networks struct {
	V4 []netV4 `json:"v4"`
}

type droplet struct {
	ID        int      `json:"id"`
	Name      string   `json:"name"`
	Memory    int      `json:"memory"`
	VCPUs     int      `json:"vcpus"`
	Disk      int      `json:"disk"`
	Region    region   `json:"region"`
	Image     image    `json:"image"`
	Size      size     `json:"size"`
	SizeSlug  string   `json:"size_slug"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"created_at"`
	Networks  networks `json:"networks"`
	Tags      []string `json:"tags"`
}

type action struct {
	ID     int    `json:"id"`
	Status string `json:"status"` // in-progress / completed / errored
	Type   string `json:"type"`
}

type linkAction struct {
	ID   int    `json:"id"`
	Rel  string `json:"rel"`
	Href string `json:"href"`
}

type links struct {
	Pages struct {
		Next string `json:"next"`
	} `json:"pages"`
	Actions []linkAction `json:"actions"`
}

type sshKey struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

// ---- 响应信封 ----

type dropletsPage struct {
	Droplets []droplet `json:"droplets"`
	Links    links     `json:"links"`
}

type dropletPage struct {
	Droplet droplet `json:"droplet"`
	Links   links   `json:"links"`
}

type actionPage struct {
	Action action `json:"action"`
}

type sshKeysPage struct {
	SSHKeys []sshKey `json:"ssh_keys"`
}

type regionsPage struct {
	Regions []region `json:"regions"`
}

type sizesPage struct {
	Sizes []size `json:"sizes"`
}

type imagesPage struct {
	Images []image `json:"images"`
}

// ---- 创建请求体 ----

type createBody struct {
	Name       string   `json:"name"`
	Region     string   `json:"region"`
	Size       string   `json:"size"`
	Image      string   `json:"image"`
	SSHKeys    []any    `json:"ssh_keys,omitempty"` // 数字 ID 或指纹串
	Tags       []string `json:"tags,omitempty"`
	UserData   string   `json:"user_data,omitempty"`
	Monitoring bool     `json:"monitoring"`
}
