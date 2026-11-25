package tencent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// newTestClient 把客户端打到 httptest 服务器；Host 头仍是产品域名（签名要求）。
func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *[]http.Request) {
	t.Helper()
	var reqs []http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body)) // 留给业务 handler 再读
		reqs = append(reqs, *r)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New("AKIDtest", "test-secret", srv.Client())
	c.endpoint = srv.URL
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c, &reqs
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiOK(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"Response": v})
	}
}

// TestCallSendsSignedRequest：实际发出的方法、头、体必须与签名所用的完全一致，
// 且 region 走 X-TC-Region 头（不进请求体），业务结果正确解码。
func TestCallSendsSignedRequest(t *testing.T) {
	c, reqs := newTestClient(t, apiOK(map[string]any{
		"TotalCount": 1,
		"ZoneSet":    []map[string]any{{"Zone": "ap-singapore-1", "ZoneName": "新加坡一区"}},
		"RequestId":  "req-1",
	}))
	var out struct {
		TotalCount int `json:"TotalCount"`
		ZoneSet    []struct {
			Zone     string `json:"Zone"`
			ZoneName string `json:"ZoneName"`
		} `json:"ZoneSet"`
	}
	err := c.Call(context.Background(), "cvm.tencentcloudapi.com", "cvm", "2017-03-12", "DescribeZones", "ap-singapore",
		map[string]any{"Filters": []map[string]any{{"Name": "zone-state", "Values": []string{"AVAILABLE"}}}}, &out)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out.TotalCount != 1 || len(out.ZoneSet) != 1 || out.ZoneSet[0].Zone != "ap-singapore-1" {
		t.Errorf("业务字段解码不符: %+v", out)
	}
	if len(*reqs) != 1 {
		t.Fatalf("请求数 = %d, want 1", len(*reqs))
	}
	r := (*reqs)[0]
	if r.Method != http.MethodPost || r.URL.Path != "/" || r.URL.RawQuery != "" {
		t.Errorf("请求行不符: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
	}
	if r.Host != "cvm.tencentcloudapi.com" {
		t.Errorf("Host 头 = %q", r.Host)
	}
	wantHeaders := map[string]string{
		"Content-Type": contentType,
		"X-TC-Action":  "DescribeZones", // 发送时保持接口名原样
		"X-TC-Version": "2017-03-12",
		"X-TC-Region":  "ap-singapore", // 公共参数走头，不进请求体
	}
	for k, want := range wantHeaders {
		if got := r.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if ts := r.Header.Get("X-TC-Timestamp"); time.Since(unixTime(ts)) > time.Minute {
		t.Errorf("X-TC-Timestamp = %q，偏离当前时间超过 1 分钟", ts)
	}
	auth := r.Header.Get("Authorization")
	if len(auth) <= len("Signature=") || auth[:len("TC3-HMAC-SHA256 Credential=AKIDtest/")] != "TC3-HMAC-SHA256 Credential=AKIDtest/" {
		t.Errorf("Authorization 不符: %q", auth)
	}
	// 用同一时刻/同一请求体重算签名，应与实际发送的一致——请求体只含业务参数，
	// 签进去的是不含 Region 的体，也就证明实际发出的体里没有 Region。
	body, _ := json.Marshal(map[string]any{
		"Filters": []map[string]any{{"Name": "zone-state", "Values": []string{"AVAILABLE"}}},
	})
	if want := signTC3("AKIDtest", "test-secret", "cvm.tencentcloudapi.com", "cvm", "DescribeZones", body, unixTime(r.Header.Get("X-TC-Timestamp"))); auth != want {
		t.Errorf("签名与请求体/时间戳不一致:\n got %s\nwant %s", auth, want)
	}
}

func unixTime(s string) time.Time {
	sec, _ := strconv.ParseInt(s, 10, 64)
	return time.Unix(sec, 0)
}

// TestCallNoRegion：region 为空时不应下发空的 Region 头，请求体为 {}。
func TestCallNoRegion(t *testing.T) {
	var gotBody string
	var gotRegion string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotRegion = r.Header.Get("X-TC-Region")
		writeJSON(w, http.StatusOK, map[string]any{"Response": map[string]any{"RequestId": "r"}})
	})
	if err := c.Call(context.Background(), "cam.tencentcloudapi.com", "cam", "2019-01-16", "GetUserAppId", "", nil, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if gotBody != "{}" {
		t.Errorf("body = %q, want {}", gotBody)
	}
	if gotRegion != "" {
		t.Errorf("X-TC-Region = %q, want 空", gotRegion)
	}
}

// TestCallAPIError：业务错误（HTTP 200 + Response.Error）应还原为 APIError，
// 可经 errors.As 取出错误码，且不重试。
func TestCallAPIError(t *testing.T) {
	c, reqs := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"Response": map[string]any{
			"RequestId": "req-e",
			"Error":     map[string]any{"Code": "AuthFailure.UnauthorizedOperation", "Message": "you are not authorized to perform operation (cvm:DescribeInstances)"},
		}})
	})
	err := c.Call(context.Background(), "cvm.tencentcloudapi.com", "cvm", "2017-03-12", "DescribeInstances", "ap-singapore", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("应返回 *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != "AuthFailure.UnauthorizedOperation" || apiErr.RequestID != "req-e" ||
		apiErr.Service != "cvm" || apiErr.Action != "DescribeInstances" || apiErr.HTTPStatus != 200 {
		t.Errorf("APIError 字段不符: %+v", *apiErr)
	}
	if len(*reqs) != 1 {
		t.Errorf("鉴权失败不应重试，请求数 = %d", len(*reqs))
	}
	if !IsCode(err, "AuthFailure.UnauthorizedOperation") || IsCode(err, "Other") || IsCode(errors.New("x"), "AuthFailure.UnauthorizedOperation") {
		t.Error("IsCode 判定不符")
	}
}

// TestCallRetries：服务端限流与网关 5xx 应退避重试，恢复后成功；持续失败在
// maxRetries 次后放弃。
func TestCallRetries(t *testing.T) {
	t.Run("限流后恢复", func(t *testing.T) {
		var sleeps []time.Duration
		n := 0
		c, reqs := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			n++
			if n <= 2 {
				writeJSON(w, http.StatusOK, map[string]any{"Response": map[string]any{
					"RequestId": "r", "Error": map[string]any{"Code": "RequestLimitExceeded", "Message": "too many requests"},
				}})
				return
			}
			apiOK(map[string]any{"RequestId": "ok"})(w, r)
		})
		c.sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
		if err := c.Call(context.Background(), "cvm.tencentcloudapi.com", "cvm", "2017-03-12", "DescribeRegions", "", nil, nil); err != nil {
			t.Fatalf("Call: %v", err)
		}
		if len(*reqs) != 3 {
			t.Errorf("请求数 = %d, want 3", len(*reqs))
		}
		if len(sleeps) != 2 || sleeps[0] != baseDelay || sleeps[1] != 2*baseDelay {
			t.Errorf("退避节奏不符: %v", sleeps)
		}
	})
	t.Run("网关5xx放弃", func(t *testing.T) {
		c, reqs := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
		})
		err := c.Call(context.Background(), "cvm.tencentcloudapi.com", "cvm", "2017-03-12", "DescribeRegions", "", nil, nil)
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
	if err := c.Call(ctx, "cvm.tencentcloudapi.com", "cvm", "2017-03-12", "DescribeRegions", "", nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("应为 context.Canceled, got %v", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("取消后不应再发请求，请求数 = %d", len(*reqs))
	}
}
