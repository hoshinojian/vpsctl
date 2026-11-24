package tencent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
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

// Client 是持有密钥对的腾讯云 API 客户端：一份密钥可跨产品调用（CVM、VPC…），并发安全。
type Client struct {
	secretID  string
	secretKey string
	// endpoint 为空表示 https://{host}/；测试注入 httptest 地址时请求打到该地址，
	// Host 头仍是产品域名（签名要求）。
	endpoint   string
	http       *http.Client
	maxRetries int
	// sleep 抽象出来以便测试退避节奏
	sleep func(ctx context.Context, d time.Duration) error
}

// New 创建客户端；hc 为 nil 时用带超时的默认客户端。
func New(secretID, secretKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Client{
		secretID:   secretID,
		secretKey:  secretKey,
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

// SetEndpoint 把请求打到指定 URL 而不是 https://{host}/（测试注入 httptest 地址用）。
// Host 头仍是产品域名——签名要求。
func (c *Client) SetEndpoint(url string) { c.endpoint = url }

// APIError 是腾讯云返回的业务错误（Response.Error）或非 API 3.0 信封的网关层错误。
// 权限不足时 Code 为 AuthFailure.UnauthorizedOperation，Message 里带缺失的
// action 与资源描述。
type APIError struct {
	Service    string
	Action     string
	Code       string // 网关层非信封错误为 "HTTP <状态码>"
	Message    string
	RequestID  string
	HTTPStatus int    // 业务错误通常经 HTTP 200 下发
	Raw        string // 响应体截断，诊断用
}

func (e *APIError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("腾讯云 %s %s: %s: %s (RequestId: %s)", e.Service, e.Action, e.Code, e.Message, e.RequestID)
	}
	return fmt.Sprintf("腾讯云 %s %s: %s: %s", e.Service, e.Action, e.Code, e.Message)
}

// IsCode 判断 err 是否为指定错误码之一的 APIError。
func IsCode(err error, codes ...string) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return slices.Contains(codes, apiErr.Code)
}

// Call 发起一次 API 3.0 调用并解码业务结果。host 为产品域名（如
// cvm.tencentcloudapi.com），service 为其产品段，version 为 API 版本（如
// 2017-03-12），action 为接口名（如 DescribeInstances）。params 为业务参数
// （数组用 []string、对象用 map[string]any）。region 非空时作为公共参数经
// X-TC-Region 头下发——地域型接口只认这个头，放在请求体里会报缺 Region。
// out 接收 Response 对象的业务字段（RequestId / Error 未声明即忽略）。
//
// 限流与 5xx 按退避重试，最多 3 次；其余错误原样返回。
func (c *Client) Call(ctx context.Context, host, service, version, action, region string, params map[string]any, out any) error {
	if params == nil {
		params = map[string]any{} // nil map 会编成 null，接口只认 JSON 对象
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("腾讯云 %s %s: 编码请求: %w", service, action, err)
	}

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, retryDelay(attempt)); err != nil {
				return err
			}
		}
		err := c.once(ctx, host, service, version, action, region, payload, out)
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
	return e.Code == "RequestLimitExceeded" || e.Code == "InternalError"
}

func (c *Client) once(ctx context.Context, host, service, version, action, region string, payload []byte, out any) error {
	now := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(host), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if c.endpoint != "" {
		req.Host = host
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-TC-Action", action)
	req.Header.Set("X-TC-Version", version)
	if region != "" {
		req.Header.Set("X-TC-Region", region)
	}
	req.Header.Set("X-TC-Timestamp", strconv.FormatInt(now.Unix(), 10))
	req.Header.Set("Authorization", signTC3(c.secretID, c.secretKey, host, service, action, payload, now))

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("腾讯云 %s %s: %w", service, action, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("腾讯云 %s %s: 读取响应: %w", service, action, err)
	}

	var env struct {
		Response json.RawMessage `json:"Response"`
	}
	if err := json.Unmarshal(data, &env); err != nil || len(env.Response) == 0 {
		return &APIError{
			Service: service, Action: action, HTTPStatus: resp.StatusCode,
			Code:    fmt.Sprintf("HTTP %d", resp.StatusCode),
			Message: snippet(data),
			Raw:     snippet(data),
		}
	}
	var head struct {
		RequestID string `json:"RequestId"`
		Error     *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
	}
	if err := json.Unmarshal(env.Response, &head); err != nil {
		return fmt.Errorf("腾讯云 %s %s: 解析响应头: %w", service, action, err)
	}
	if head.Error != nil {
		return &APIError{
			Service: service, Action: action, HTTPStatus: resp.StatusCode,
			Code: head.Error.Code, Message: head.Error.Message,
			RequestID: head.RequestID, Raw: snippet(data),
		}
	}
	if out != nil {
		if err := json.Unmarshal(env.Response, out); err != nil {
			return fmt.Errorf("腾讯云 %s %s: 解码响应: %w", service, action, err)
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

// ClientToken 生成幂等令牌：名字片段 + 随机串，≤64 个 ASCII 字符。客户端遇 5xx 重试时
// 同一个令牌不会被平台当成两次购买——对建一次就付一个月的产品尤其要紧。
func ClientToken(name string) string {
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
