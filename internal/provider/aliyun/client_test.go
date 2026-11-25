package aliyun

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient 把客户端打到 httptest 服务器；Host 头仍是产品域名（签名要求）。
func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *[]http.Request) {
	t.Helper()
	var reqs []http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, *r)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New("AKIDtest", "test-secret", srv.Client())
	c.endpoint = srv.URL
	c.nonce = func() string { return "fixed-nonce" }
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c, &reqs
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// TestCallSendsSignedRequest：实际发出的方法、query、头必须与签名所用的完全一致，
// 且业务结果正确解码。
func TestCallSendsSignedRequest(t *testing.T) {
	c, reqs := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"RequestId": "req-1", "TotalCount": 1,
			"Zones": map[string]any{"Zone": []map[string]any{{"ZoneId": "cn-hangzhou-b", "LocalName": "杭州可用区B"}}},
		})
	})
	var out struct {
		Zones struct {
			Zone []struct {
				ZoneId    string `json:"ZoneId"`
				LocalName string `json:"LocalName"`
			} `json:"Zone"`
		} `json:"Zones"`
	}
	err := c.Call(context.Background(), "ecs.cn-hangzhou.aliyuncs.com", "2014-05-26", "DescribeZones",
		map[string]string{"RegionId": "cn-hangzhou", "ZoneId": "", "Action": "x y/z"}, &out)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(out.Zones.Zone) != 1 || out.Zones.Zone[0].ZoneId != "cn-hangzhou-b" {
		t.Errorf("业务字段解码不符: %+v", out)
	}
	if len(*reqs) != 1 {
		t.Fatalf("请求数 = %d, want 1", len(*reqs))
	}
	r := (*reqs)[0]
	const wantQuery = "Action=x%20y%2Fz&RegionId=cn-hangzhou" // 空值 ZoneId 丢弃
	if r.Method != http.MethodGet || r.URL.Path != "/" || r.URL.RawQuery != wantQuery || r.Host != "ecs.cn-hangzhou.aliyuncs.com" {
		t.Errorf("请求行不符: %s %s?%s Host=%s", r.Method, r.URL.Path, r.URL.RawQuery, r.Host)
	}
	if r.ContentLength != 0 {
		t.Errorf("请求体应为空, ContentLength = %d", r.ContentLength)
	}
	wantHeaders := map[string]string{
		"X-Acs-Action":          "DescribeZones",
		"X-Acs-Version":         "2014-05-26",
		"X-Acs-Content-Sha256":  emptyBodyHash,
		"X-Acs-Signature-Nonce": "fixed-nonce",
	}
	for k, want := range wantHeaders {
		if got := r.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	date := r.Header.Get("X-Acs-Date")
	if _, err := time.Parse("2006-01-02T15:04:05Z", date); err != nil {
		t.Errorf("X-Acs-Date = %q，不是 UTC ISO 8601: %v", date, err)
	}
	wantHeaders["X-Acs-Date"] = date
	// 除签名头外不应有任何 x-acs-* 头（多发一个就会 SignatureNotMatch）
	for k := range r.Header {
		if strings.HasPrefix(k, "X-Acs-") {
			if _, signed := wantHeaders[k]; !signed {
				t.Errorf("发送了未签名的头 %s", k)
			}
		}
	}
	want := signACS3(http.MethodGet, "AKIDtest", "test-secret", "ecs.cn-hangzhou.aliyuncs.com", wantQuery,
		"DescribeZones", "2014-05-26", date, "fixed-nonce")
	if got := r.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization 不符:\n got %s\nwant %s", got, want)
	}
}

// TestCallAPIError：非 2xx 的 JSON 错误应还原为 APIError，可经 errors.As 取出，
// 且不重试。
func TestCallAPIError(t *testing.T) {
	c, reqs := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"RequestId":"req-e","Code":"Forbidden.RAM","Message":"User is not authorized to do this action."}`))
	})
	err := c.Call(context.Background(), "ecs.aliyuncs.com", "2014-05-26", "DescribeInstances",
		map[string]string{"RegionId": "cn-hangzhou"}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("应返回 *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != "Forbidden.RAM" || apiErr.RequestID != "req-e" ||
		apiErr.Action != "DescribeInstances" || apiErr.HTTPStatus != http.StatusForbidden {
		t.Errorf("APIError 字段不符: %+v", *apiErr)
	}
	if len(*reqs) != 1 {
		t.Errorf("鉴权失败不应重试，请求数 = %d", len(*reqs))
	}
	if !IsCode(err, "Forbidden.RAM") || IsCode(err, "Other") || IsCode(errors.New("x"), "Forbidden.RAM") {
		t.Error("IsCode 判定不符")
	}
}

// TestCallRetries：限流与 5xx 应退避重试，恢复后成功；持续失败在 maxRetries 次后放弃。
func TestCallRetries(t *testing.T) {
	t.Run("限流后恢复", func(t *testing.T) {
		var sleeps []time.Duration
		n := 0
		c, reqs := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			n++
			if n == 1 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"Code": "Throttling.User", "Message": "Requests rate too high"})
				return
			}
			if n == 2 {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"Code": "ServiceUnavailable", "Message": "try later"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"RequestId": "ok"})
		})
		c.sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
		if err := c.Call(context.Background(), "ecs.aliyuncs.com", "2014-05-26", "DescribeRegions", nil, nil); err != nil {
			t.Fatalf("Call: %v", err)
		}
		if len(*reqs) != 3 || len(sleeps) != 2 || sleeps[0] != baseDelay || sleeps[1] != 2*baseDelay {
			t.Errorf("请求数 %d / 退避 %v，want 3 / [%v %v]", len(*reqs), sleeps, baseDelay, 2*baseDelay)
		}
	})
	t.Run("非JSON的5xx放弃", func(t *testing.T) {
		c, reqs := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>502</html>"))
		})
		err := c.Call(context.Background(), "ecs.aliyuncs.com", "2014-05-26", "DescribeRegions", nil, nil)
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusBadGateway || apiErr.Code != "HTTP 502" {
			t.Fatalf("应为网关层 APIError, got %v", err)
		}
		if len(*reqs) != maxRetries+1 {
			t.Errorf("请求数 = %d, want %d", len(*reqs), maxRetries+1)
		}
	})
}

// TestCallCancel：上下文取消后立即返回，不再重试。
func TestCallCancel(t *testing.T) {
	c, reqs := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ctx, cancel := context.WithCancel(context.Background())
	c.sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	if err := c.Call(ctx, "ecs.aliyuncs.com", "2014-05-26", "DescribeRegions", nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("应为 context.Canceled, got %v", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("取消后不应再发请求，请求数 = %d", len(*reqs))
	}
}

func TestRandomNonce(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		n := randomNonce()
		if len(n) != 32 {
			t.Fatalf("nonce 长度 = %d, want 32: %q", len(n), n)
		}
		if seen[n] {
			t.Fatalf("nonce 重复: %s", n)
		}
		seen[n] = true
	}
}
