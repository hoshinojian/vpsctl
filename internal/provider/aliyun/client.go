package aliyun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

const (
	maxRetries   = 3
	baseDelay    = 500 * time.Millisecond
	maxRetryWait = 30 * time.Second
	maxBodyBytes = 32 << 20 // 响应体上限，防异常响应撑爆内存
)

// defaultHTTPTimeout 单请求总超时。上游网络挂起（丢包/代理吞连）时若无超时，
// 调用会永久阻塞，拖死管理台与 CLI。
const defaultHTTPTimeout = 15 * time.Second

// Client 是持有密钥对的阿里云 OpenAPI 客户端：一份密钥可跨产品调用（ECS、VPC…），并发安全。
type Client struct {
	accessKeyID     string
	accessKeySecret string
	// endpoint 为空表示 https://{host}/；测试注入 httptest 地址时请求打到该地址，
	// Host 头仍是产品域名（签名要求）。
	endpoint   string
	http       *http.Client
	maxRetries int
	// nonce 生成每次请求唯一的防重放串；sleep 抽象出来以便测试退避节奏
	nonce func() string
	sleep func(ctx context.Context, d time.Duration) error
}

// New 创建客户端；hc 为 nil 时用带超时的默认客户端。
func New(accessKeyID, accessKeySecret string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Client{
		accessKeyID:     accessKeyID,
		accessKeySecret: accessKeySecret,
		http:            hc,
		maxRetries:      maxRetries,
		nonce:           randomNonce,
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

// SetEndpoint 把请求打到指定 URL 而不是 https://{host}/（测试注入 httptest 地址用）。
// Host 头仍是产品域名——签名要求。
func (c *Client) SetEndpoint(url string) { c.endpoint = url }

func randomNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// APIError 是阿里云返回的业务错误（非 2xx + Code/Message）。权限不足时 Code 为
// Forbidden.RAM / NoPermission 一类，Message 里带被拒的 action。
type APIError struct {
	Action     string
	Code       string // 非 JSON 响应为 "HTTP <状态码>"
	Message    string
	RequestID  string
	HTTPStatus int
	Raw        string // 响应体截断，诊断用
}

func (e *APIError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("阿里云 %s: %s: %s (RequestId: %s)", e.Action, e.Code, e.Message, e.RequestID)
	}
	return fmt.Sprintf("阿里云 %s: %s: %s", e.Action, e.Code, e.Message)
}

// IsCode 判断 err 是否为指定错误码之一的 APIError。
func IsCode(err error, codes ...string) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return slices.Contains(codes, apiErr.Code)
}

// Call 发起一次 RPC 调用并解码结果。host 为产品域名（如 ecs.aliyuncs.com），
// version 为 API 版本（如 2014-05-26），action 为接口名（如 DescribeInstances）。
// params 为业务参数（含 RegionId 等地域参数；数组按下标平铺，如 Tag.1.Key），
// 空值参数自动丢弃。out 接收响应 JSON 中声明的字段。
//
// 限流与 5xx 按退避重试，最多 3 次；其余错误原样返回。
func (c *Client) Call(ctx context.Context, host, version, action string, params map[string]string, out any) error {
	query := canonicalQuery(params)

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, retryDelay(attempt)); err != nil {
				return err
			}
		}
		err := c.once(ctx, host, version, action, query, out)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && !retryable(apiErr) {
			return err
		}
		if attempt >= c.maxRetries {
			return err
		}
	}
}

// retryable 判定失败是否值得重试：传输错误（非 APIError）、网关限流/5xx、
// 服务端限流与内部错误。鉴权/参数类错误重试无意义。
func retryable(e *APIError) bool {
	if e.HTTPStatus == http.StatusTooManyRequests || e.HTTPStatus >= 500 {
		return true
	}
	return strings.HasPrefix(e.Code, "Throttling") ||
		e.Code == "InternalError" || e.Code == "ServiceUnavailable" || e.Code == "ServerUnavailable"
}

func (c *Client) once(ctx context.Context, host, version, action, query string, out any) error {
	date := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	nonce := c.nonce()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(host)+"?"+query, nil)
	if err != nil {
		return err
	}
	if c.endpoint != "" {
		req.Host = host
	}
	req.Header.Set("X-Acs-Action", action)
	req.Header.Set("X-Acs-Version", version)
	req.Header.Set("X-Acs-Date", date)
	req.Header.Set("X-Acs-Signature-Nonce", nonce)
	req.Header.Set("X-Acs-Content-Sha256", emptyBodyHash)
	req.Header.Set("Authorization", signACS3(http.MethodGet, c.accessKeyID, c.accessKeySecret, host, query, action, version, date, nonce))

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("阿里云 %s: %w", action, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("阿里云 %s: 读取响应: %w", action, err)
	}

	apiErr := &APIError{Action: action, HTTPStatus: resp.StatusCode, Raw: snippet(data)}
	var head struct {
		RequestID string `json:"RequestId"`
		Code      string `json:"Code"`
		Message   string `json:"Message"`
	}
	jsonErr := json.Unmarshal(data, &head)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if jsonErr != nil || head.Code == "" {
			apiErr.Code = fmt.Sprintf("HTTP %d", resp.StatusCode)
			apiErr.Message = snippet(data)
			return apiErr
		}
		apiErr.Code, apiErr.Message, apiErr.RequestID = head.Code, head.Message, head.RequestID
		return apiErr
	}
	if jsonErr != nil {
		return fmt.Errorf("阿里云 %s: 解码响应: %w（响应体: %s）", action, jsonErr, snippet(data))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("阿里云 %s: 解码响应: %w", action, err)
		}
	}
	return nil
}

func (c *Client) url(host string) string {
	if c.endpoint != "" {
		return c.endpoint
	}
	return "https://" + host + "/"
}

// retryDelay 计算第 attempt 次重试（从 1 计）前的等待时长：指数退避，封顶 30s。
func retryDelay(attempt int) time.Duration {
	d := baseDelay << (attempt - 1)
	if d <= 0 || d > maxRetryWait {
		return maxRetryWait
	}
	return d
}

func snippet(b []byte) string {
	return strings.TrimSpace(string(b[:min(len(b), 1024)]))
}
