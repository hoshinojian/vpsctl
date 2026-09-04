package digitalocean

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
)

const (
	defaultBaseURL = "https://api.digitalocean.com/v2"
	perPage        = 200
	maxPages       = 100 // 安全上限，防分页死循环
	maxRetries     = 3
	baseDelay      = 500 * time.Millisecond
	maxRetryWait   = 30 * time.Second
)

// Client 是单账号的 DigitalOcean API 客户端，并发安全。
type Client struct {
	account    string
	token      string
	baseURL    string
	http       *http.Client
	maxRetries int
	// sleep 抽象出来以便测试 429 退避节奏
	sleep func(ctx context.Context, d time.Duration) error
}

// New 创建客户端；hc 为 nil 时用 http.DefaultClient。
func New(account, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{
		account:    account,
		token:      token,
		baseURL:    defaultBaseURL,
		http:       hc,
		maxRetries: maxRetries,
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
}

func init() {
	provider.Register("digitalocean", func(account, token string, hc *http.Client) (provider.Provider, error) {
		if token == "" {
			return nil, errors.New("digitalocean: token 为空")
		}
		return New(account, token, hc), nil
	})
}

// APIError 是非 2xx 响应。
type APIError struct {
	Method     string
	Path       string
	Status     int
	Body       string // 截断到 256 字节
	RetryAfter string // Retry-After 头原值
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// do 发送请求并解析 JSON 响应；out 非 nil 且响应非 204 时解码。
// 429 与 5xx、网络错误按退避重试，最多 maxRetries 次。
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("编码请求: %w", err)
		}
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, retryDelay(lastErr, attempt)); err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			lastErr = err
			if attempt >= c.maxRetries {
				return fmt.Errorf("%s %s: %w", method, path, err)
			}
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out != nil && resp.StatusCode != http.StatusNoContent {
				if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
					resp.Body.Close()
					return fmt.Errorf("%s %s: 解码响应: %w", method, path, err)
				}
			}
			resp.Body.Close()
			return nil
		}
		apiErr := &APIError{
			Method:     method,
			Path:       path,
			Status:     resp.StatusCode,
			Body:       readSnippet(resp.Body),
			RetryAfter: resp.Header.Get("Retry-After"),
		}
		resp.Body.Close()
		if (apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= 500) && attempt < c.maxRetries {
			lastErr = apiErr
			continue
		}
		return apiErr
	}
}

// retryDelay 计算第 attempt 次重试（从 1 计）前的等待时长。
func retryDelay(err error, attempt int) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if d := retryAfterDuration(apiErr.RetryAfter); d > 0 {
			return min(d, maxRetryWait)
		}
	}
	d := baseDelay << (attempt - 1)
	if d <= 0 || d > maxRetryWait {
		return maxRetryWait
	}
	return d
}

func retryAfterDuration(s string) time.Duration {
	sec, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || sec < 0 {
		return 0
	}
	return time.Duration(sec) * time.Second
}

func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 256))
	return strings.TrimSpace(string(b))
}

func (c *Client) List(ctx context.Context) ([]provider.Server, error) {
	var out []provider.Server
	for page := 1; page <= maxPages; page++ {
		var pg dropletsPage
		path := fmt.Sprintf("/droplets?per_page=%d&page=%d", perPage, page)
		if err := c.do(ctx, http.MethodGet, path, nil, &pg); err != nil {
			return nil, err
		}
		for _, d := range pg.Droplets {
			out = append(out, c.toServer(d))
		}
		if len(pg.Droplets) < perPage {
			return out, nil
		}
	}
	return out, fmt.Errorf("droplets 超过 %d 页（%d 台），放弃遍历", maxPages, perPage*maxPages)
}

func (c *Client) Get(ctx context.Context, id string) (provider.Server, error) {
	var pg dropletPage
	if err := c.do(ctx, http.MethodGet, "/droplets/"+url.PathEscape(id), nil, &pg); err != nil {
		return provider.Server{}, err
	}
	return c.toServer(pg.Droplet), nil
}

// Create 提交创建请求并立即返回（DO 为 202，节点随后进入 new → active）。
func (c *Client) Create(ctx context.Context, req provider.CreateRequest) (provider.Server, error) {
	keys, err := c.resolveSSHKeys(ctx, req.SSHKeys)
	if err != nil {
		return provider.Server{}, err
	}
	body := createBody{
		Name:       req.Name,
		Region:     req.Region,
		Size:       req.Size,
		Image:      req.Image,
		SSHKeys:    keys,
		Tags:       req.Tags,
		UserData:   req.UserData,
		Monitoring: req.Monitoring,
	}
	var pg dropletPage
	if err := c.do(ctx, http.MethodPost, "/droplets", body, &pg); err != nil {
		return provider.Server{}, err
	}
	return c.toServer(pg.Droplet), nil
}

// resolveSSHKeys 把混合输入（数字 ID / MD5 指纹 / 名称）归一为 DO 接受的
// 数字 ID 或指纹；名称经账号公钥列表匹配解析。
func (c *Client) resolveSSHKeys(ctx context.Context, in []string) ([]any, error) {
	if len(in) == 0 {
		return nil, nil
	}
	var out []any
	var byName []string
	for _, k := range in {
		switch {
		case isDigits(k):
			n, err := strconv.Atoi(k)
			if err != nil {
				return nil, fmt.Errorf("ssh key id %q: %w", k, err)
			}
			out = append(out, n)
		case strings.Contains(k, ":"):
			out = append(out, k)
		default:
			byName = append(byName, k)
		}
	}
	if len(byName) == 0 {
		return out, nil
	}
	keys, err := c.SSHKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("解析 ssh key 名称 %v: %w", byName, err)
	}
	known := make(map[string]provider.SSHKey, len(keys))
	for _, k := range keys {
		known[k.Name] = k
	}
	for _, name := range byName {
		k, ok := known[name]
		if !ok {
			return nil, fmt.Errorf("ssh key %q 不存在于账号 %s", name, c.account)
		}
		if n, err := strconv.Atoi(k.ID); err == nil {
			out = append(out, n)
		} else {
			out = append(out, k.Fingerprint)
		}
	}
	return out, nil
}

// Delete 删除节点，DO 对运行中节点直接生效。
// 已不存在（404）视为成功，便于批量操作幂等重试。
func (c *Client) Delete(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/droplets/"+url.PathEscape(id), nil, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// Power 发起电源动作（power_off / power_on / shutdown），异步返回句柄。
func (c *Client) Power(ctx context.Context, id, action string) (provider.ActionRef, error) {
	switch action {
	case provider.PowerOff, provider.PowerOn, provider.Shutdown:
	default:
		return provider.ActionRef{}, fmt.Errorf("digitalocean: 不支持的动作 %q", action)
	}
	path := "/droplets/" + url.PathEscape(id) + "/actions"
	var pg actionPage
	if err := c.do(ctx, http.MethodPost, path, map[string]string{"type": action}, &pg); err != nil {
		return provider.ActionRef{}, err
	}
	return provider.ActionRef{ID: strconv.Itoa(pg.Action.ID)}, nil
}

func (c *Client) ActionStatus(ctx context.Context, ref provider.ActionRef) (string, error) {
	var pg actionPage
	if err := c.do(ctx, http.MethodGet, "/actions/"+url.PathEscape(ref.ID), nil, &pg); err != nil {
		return "", err
	}
	return pg.Action.Status, nil
}

func (c *Client) SSHKeys(ctx context.Context) ([]provider.SSHKey, error) {
	var pg sshKeysPage
	if err := c.do(ctx, http.MethodGet, "/account/keys?per_page=200", nil, &pg); err != nil {
		return nil, err
	}
	out := make([]provider.SSHKey, 0, len(pg.SSHKeys))
	for _, k := range pg.SSHKeys {
		out = append(out, provider.SSHKey{
			ID:          strconv.Itoa(k.ID),
			Name:        k.Name,
			Fingerprint: k.Fingerprint,
		})
	}
	return out, nil
}

func (c *Client) Regions(ctx context.Context) ([]provider.Region, error) {
	var out []provider.Region
	for page := 1; page <= maxPages; page++ {
		var pg regionsPage
		path := fmt.Sprintf("/regions?per_page=%d&page=%d", perPage, page)
		if err := c.do(ctx, http.MethodGet, path, nil, &pg); err != nil {
			return nil, err
		}
		for _, r := range pg.Regions {
			if r.Available {
				out = append(out, provider.Region{Slug: r.Slug, Name: r.Name})
			}
		}
		if len(pg.Regions) < perPage {
			return out, nil
		}
	}
	return out, nil
}

func (c *Client) Sizes(ctx context.Context) ([]provider.Size, error) {
	var out []provider.Size
	for page := 1; page <= maxPages; page++ {
		var pg sizesPage
		path := fmt.Sprintf("/sizes?per_page=%d&page=%d", perPage, page)
		if err := c.do(ctx, http.MethodGet, path, nil, &pg); err != nil {
			return nil, err
		}
		for _, s := range pg.Sizes {
			if s.Available {
				out = append(out, provider.Size{
					Slug:         s.Slug,
					VCPUs:        s.VCPUs,
					MemoryMB:     s.Memory,
					DiskGB:       s.Disk,
					PriceMonthly: s.PriceMonthly,
				})
			}
		}
		if len(pg.Sizes) < perPage {
			return out, nil
		}
	}
	return out, nil
}

func (c *Client) Images(ctx context.Context) ([]provider.Image, error) {
	var out []provider.Image
	for page := 1; page <= maxPages; page++ {
		var pg imagesPage
		path := fmt.Sprintf("/images?type=distribution&per_page=%d&page=%d", perPage, page)
		if err := c.do(ctx, http.MethodGet, path, nil, &pg); err != nil {
			return nil, err
		}
		for _, im := range pg.Images {
			if im.Slug == "" {
				continue
			}
			out = append(out, provider.Image{Slug: im.Slug, Distribution: im.Distribution, Name: im.Name})
		}
		if len(pg.Images) < perPage {
			return out, nil
		}
	}
	return out, nil
}

func (c *Client) toServer(d droplet) provider.Server {
	s := provider.Server{
		ID:           strconv.Itoa(d.ID),
		Account:      c.account,
		Name:         d.Name,
		Status:       d.Status,
		Region:       d.Region.Slug,
		Size:         d.SizeSlug,
		Image:        d.Image.Slug,
		VCPUs:        d.VCPUs,
		MemoryMB:     d.Memory,
		DiskGB:       d.Disk,
		PriceMonthly: d.Size.PriceMonthly,
		Tags:         d.Tags,
	}
	if t, err := time.Parse(time.RFC3339, d.CreatedAt); err == nil {
		s.CreatedAt = t
	}
	for _, n := range d.Networks.V4 {
		switch n.Type {
		case "public":
			s.IPv4Public = n.IPAddress
		case "private":
			s.IPv4Private = n.IPAddress
		}
	}
	return s
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
