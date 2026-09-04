package digitalocean

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
)

func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("acct-1", "test-token", srv.Client())
	c.baseURL = srv.URL
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func testDroplet(id int) droplet {
	return droplet{
		ID: id, Name: fmt.Sprintf("node-%02d", id), Memory: 1024, VCPUs: 1, Disk: 25,
		Region:    region{Slug: "sgp1", Name: "Singapore 1", Available: true},
		Image:     image{Slug: "ubuntu-24-04-x64", Distribution: "Ubuntu", Name: "24.04 x64"},
		Size:      size{Slug: "s-1vcpu-1gb", PriceMonthly: 6},
		SizeSlug:  "s-1vcpu-1gb",
		Status:    "active",
		CreatedAt: "2026-09-04T00:00:00Z",
		Networks: networks{V4: []netV4{
			{IPAddress: "203.0.113.10", Type: "public"},
			{IPAddress: "10.116.0.2", Type: "private"},
		}},
		Tags: []string{"batch:x"},
	}
}

// sshKeyAsFloat 取 ssh_keys 首元素：JSON 数字经 encoding/json 解码为 float64。
func sshKeyAsFloat(keys []any) (float64, bool) {
	if len(keys) != 1 {
		return 0, false
	}
	n, ok := keys[0].(float64)
	return n, ok
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestAuthHeader(t *testing.T) {
	var got string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		writeJSON(w, http.StatusOK, dropletsPage{})
	}))
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer test-token" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestListPagination(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /droplets", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		var items []droplet
		switch page {
		case "1":
			for i := 1; i <= perPage; i++ {
				items = append(items, testDroplet(i))
			}
		case "2":
			items = []droplet{testDroplet(1000)}
		default:
			t.Errorf("意外页码 %q", page)
		}
		pg := dropletsPage{Droplets: items}
		if page == "1" {
			pg.Links.Pages.Next = "https://api.digitalocean.com/v2/droplets?page=2"
		}
		writeJSON(w, http.StatusOK, pg)
	})
	c := newTestClient(t, mux)

	got, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != perPage+1 {
		t.Fatalf("共 %d 台, want %d", len(got), perPage+1)
	}
	s := got[0]
	if s.ID != "1" || s.Account != "acct-1" || s.Status != "active" ||
		s.Region != "sgp1" || s.Size != "s-1vcpu-1gb" || s.Image != "ubuntu-24-04-x64" ||
		s.IPv4Public != "203.0.113.10" || s.IPv4Private != "10.116.0.2" ||
		s.PriceMonthly != 6 || s.MemoryMB != 1024 || s.VCPUs != 1 || s.DiskGB != 25 {
		t.Errorf("字段映射不符: %+v", s)
	}
	if s.CreatedAt.IsZero() {
		t.Error("CreatedAt 未解析")
	}
	if got[perPage].ID != "1000" {
		t.Errorf("第二页首条 ID = %q", got[perPage].ID)
	}
}

func TestCreate(t *testing.T) {
	var gotBody createBody
	mux := http.NewServeMux()
	mux.HandleFunc("POST /droplets", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		d := testDroplet(1001)
		d.Status = "new"
		d.Networks.V4 = nil // 刚提交时还没有 IP
		writeJSON(w, http.StatusAccepted, dropletPage{Droplet: d})
	})
	c := newTestClient(t, mux)

	s, err := c.Create(context.Background(), provider.CreateRequest{
		Name: "node-a", Region: "sgp1", Size: "s-1vcpu-1gb", Image: "ubuntu-24-04-x64",
		SSHKeys: []string{"99"}, Tags: []string{"b1"},
		UserData: "#cloud-config", Monitoring: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody.Name != "node-a" || gotBody.Region != "sgp1" ||
		gotBody.Size != "s-1vcpu-1gb" || gotBody.Image != "ubuntu-24-04-x64" {
		t.Errorf("请求体不符: %+v", gotBody)
	}
	if n, ok := sshKeyAsFloat(gotBody.SSHKeys); !ok || n != 99 {
		t.Errorf("ssh_keys = %v, want [99]", gotBody.SSHKeys)
	}
	if !gotBody.Monitoring || len(gotBody.Tags) != 1 || gotBody.Tags[0] != "b1" || gotBody.UserData != "#cloud-config" {
		t.Errorf("请求体不符: %+v", gotBody)
	}
	if s.ID != "1001" || s.Status != "new" || s.Account != "acct-1" || s.IPv4Public != "" {
		t.Errorf("返回值不符: %+v", s)
	}
}

func TestCreateResolvesKeyName(t *testing.T) {
	var gotBody createBody
	mux := http.NewServeMux()
	mux.HandleFunc("GET /account/keys", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, sshKeysPage{SSHKeys: []sshKey{
			{ID: 99, Name: "laptop", Fingerprint: "aa:bb:cc"},
		}})
	})
	mux.HandleFunc("POST /droplets", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, http.StatusAccepted, dropletPage{Droplet: testDroplet(1)})
	})
	c := newTestClient(t, mux)

	_, err := c.Create(context.Background(), provider.CreateRequest{
		Name: "n", Region: "sgp1", Size: "s", Image: "img", SSHKeys: []string{"laptop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := sshKeyAsFloat(gotBody.SSHKeys); !ok || n != 99 {
		t.Errorf("名称未解析为 ID: %v", gotBody.SSHKeys)
	}

	_, err = c.Create(context.Background(), provider.CreateRequest{
		Name: "n", Region: "sgp1", Size: "s", Image: "img", SSHKeys: []string{"no-such-key"},
	})
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("未知 key 名应报错: %v", err)
	}
}

func TestDelete(t *testing.T) {
	t.Run("204 成功", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("DELETE /droplets/7", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		if err := newTestClient(t, mux).Delete(context.Background(), "7"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("404 视为已删除", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("DELETE /droplets/7", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, map[string]string{"id": "not_found"})
		})
		if err := newTestClient(t, mux).Delete(context.Background(), "7"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("500 重试耗尽", func(t *testing.T) {
		attempts := 0
		mux := http.NewServeMux()
		mux.HandleFunc("DELETE /droplets/7", func(w http.ResponseWriter, r *http.Request) {
			attempts++
			writeJSON(w, http.StatusInternalServerError, map[string]string{"id": "server_error"})
		})
		err := newTestClient(t, mux).Delete(context.Background(), "7")
		if err == nil {
			t.Fatal("应报错")
		}
		if attempts != maxRetries+1 {
			t.Errorf("attempts = %d, want %d", attempts, maxRetries+1)
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
			t.Errorf("错误类型不符: %v", err)
		}
	})
}

func Test429HonorsRetryAfter(t *testing.T) {
	var delays []time.Duration
	attempts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /droplets", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "7")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"id": "too_many_requests"})
			return
		}
		writeJSON(w, http.StatusOK, dropletsPage{})
	})
	c := newTestClient(t, mux)
	c.sleep = func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}

	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || len(delays) != 1 || delays[0] != 7*time.Second {
		t.Errorf("attempts=%d delays=%v", attempts, delays)
	}
}

func TestGetNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /droplets/404", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"id": "not_found", "message": "not found"})
	})
	_, err := newTestClient(t, mux).Get(context.Background(), "404")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("应返回 APIError 404, got %v", err)
	}
}

func TestPowerAndStatus(t *testing.T) {
	var gotType string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /droplets/5/actions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotType = body["type"]
		writeJSON(w, http.StatusCreated, actionPage{Action: action{ID: 42, Status: "in-progress", Type: gotType}})
	})
	mux.HandleFunc("GET /actions/42", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, actionPage{Action: action{ID: 42, Status: "completed"}})
	})
	c := newTestClient(t, mux)

	ref, err := c.Power(context.Background(), "5", provider.PowerOff)
	if err != nil {
		t.Fatal(err)
	}
	if gotType != provider.PowerOff || ref.ID != "42" {
		t.Errorf("type=%q ref=%+v", gotType, ref)
	}
	st, err := c.ActionStatus(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if st != "completed" {
		t.Errorf("status = %q", st)
	}
	if _, err := c.Power(context.Background(), "5", "reboot-xyz"); err == nil {
		t.Error("不支持的动应报错")
	}
}

func TestRegionsFiltersUnavailable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /regions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, regionsPage{Regions: []region{
			{Slug: "sgp1", Name: "Singapore 1", Available: true},
			{Slug: "tor1", Name: "Toronto 1", Available: false},
		}})
	})
	got, err := newTestClient(t, mux).Regions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Slug != "sgp1" {
		t.Errorf("Regions = %+v", got)
	}
}

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		attempt int
		want    time.Duration
	}{
		{"429 按 Retry-After", &APIError{Status: 429, RetryAfter: "3"}, 1, 3 * time.Second},
		{"429 Retry-After 超上限截断", &APIError{Status: 429, RetryAfter: "3600"}, 1, maxRetryWait},
		{"5xx 首次指数 0.5s", &APIError{Status: 500}, 1, 500 * time.Millisecond},
		{"5xx 第二次 1s", &APIError{Status: 500}, 2, time.Second},
		{"Retry-After 非法退回指数", &APIError{Status: 429, RetryAfter: "soon"}, 2, time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryDelay(tc.err, tc.attempt); got != tc.want {
				t.Errorf("retryDelay = %v, want %v", got, tc.want)
			}
		})
	}
}
