package digitalocean

// tag_test.go：Tag 能力——tag 先建后挂、幂等摘除、tag 名本地校验、并发首建只发一次。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// tagAPI 是 /tags 端点桩：记录写调用，可让指定 tag 的挂载/摘除失败。
type tagAPI struct {
	mu       sync.Mutex
	existing map[string]bool // 账号里已存在的 tag
	created  []string        // POST /tags 建过的 tag
	assigned []string        // "tag>id"
	removed  []string        // "tag>id"
	failTag  string          // 该 tag 的挂载请求返回 403（token 缺 droplet:update 之类）
	hits     int             // 收到的请求数
}

func (a *tagAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tags/{name}", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.hits++
		name := r.PathValue("name")
		if !a.existing[name] {
			writeJSON(w, http.StatusNotFound, map[string]string{"id": "not_found"})
			return
		}
		writeJSON(w, http.StatusOK, tagPage{Tag: tagName{Name: name}})
	})
	mux.HandleFunc("POST /tags", func(w http.ResponseWriter, r *http.Request) {
		var body tagName
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.mu.Lock()
		defer a.mu.Unlock()
		a.hits++
		a.created = append(a.created, body.Name)
		a.existing[body.Name] = true
		writeJSON(w, http.StatusCreated, tagPage{Tag: body})
	})
	mux.HandleFunc("POST /tags/{name}/resources", func(w http.ResponseWriter, r *http.Request) {
		var body tagResourcesBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		name := r.PathValue("name")
		a.mu.Lock()
		defer a.mu.Unlock()
		a.hits++
		if len(body.Resources) != 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "resources"})
			return
		}
		a.assigned = append(a.assigned, name+">"+body.Resources[0].ResourceID)
		if body.Resources[0].ResourceType != "droplet" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "resource_type"})
			return
		}
		if name == a.failTag {
			writeJSON(w, http.StatusForbidden, map[string]string{"id": "forbidden"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /tags/{name}/resources", func(w http.ResponseWriter, r *http.Request) {
		var body tagResourcesBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		name := r.PathValue("name")
		a.mu.Lock()
		defer a.mu.Unlock()
		a.hits++
		a.removed = append(a.removed, name+">"+body.Resources[0].ResourceID)
		if !a.existing[name] {
			writeJSON(w, http.StatusNotFound, map[string]string{"id": "not_found"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func newTagClient(t *testing.T, existing ...string) (*Client, *tagAPI) {
	t.Helper()
	a := &tagAPI{existing: map[string]bool{}}
	for _, e := range existing {
		a.existing[e] = true
	}
	return newTestClient(t, a.handler()), a
}

func TestTagAddCreatesMissingTagThenAssigns(t *testing.T) {
	c, a := newTagClient(t)
	if err := c.Tag(context.Background(), "7", []string{"managed-by:demo-nms"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(a.created) != 1 || a.created[0] != "managed-by:demo-nms" {
		t.Errorf("新 tag 应先创建一次: %v", a.created)
	}
	if len(a.assigned) != 1 || a.assigned[0] != "managed-by:demo-nms>7" {
		t.Errorf("应挂到 droplet 7: %v", a.assigned)
	}
	// 同一客户端再挂：tag 已确认存在，不再重复创建
	if err := c.Tag(context.Background(), "8", []string{"managed-by:demo-nms"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(a.created) != 1 || len(a.assigned) != 2 {
		t.Errorf("第二次不应再建 tag: created=%v assigned=%v", a.created, a.assigned)
	}
}

func TestTagAddExistingTagSkipsCreate(t *testing.T) {
	c, a := newTagClient(t, "nms-prod")
	if err := c.Tag(context.Background(), "7", []string{"nms-prod"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(a.created) != 0 || len(a.assigned) != 1 {
		t.Errorf("已存在的 tag 不应再建: created=%v assigned=%v", a.created, a.assigned)
	}
}

func TestTagRemove(t *testing.T) {
	t.Run("摘除已挂的 tag", func(t *testing.T) {
		c, a := newTagClient(t, "old")
		if err := c.Tag(context.Background(), "7", nil, []string{"old"}); err != nil {
			t.Fatal(err)
		}
		if len(a.removed) != 1 || a.removed[0] != "old>7" {
			t.Errorf("removed = %v", a.removed)
		}
	})
	t.Run("tag 已不存在视为已摘除", func(t *testing.T) {
		c, _ := newTagClient(t)
		if err := c.Tag(context.Background(), "7", nil, []string{"gone"}); err != nil {
			t.Fatalf("404 应幂等成功: %v", err)
		}
	})
}

func TestTagPartialFailureNamesDonePart(t *testing.T) {
	a := &tagAPI{existing: map[string]bool{"first": true}, failTag: "second"}
	err := newTestClient(t, a.handler()).Tag(context.Background(), "7", []string{"first", "second"}, nil)
	if err == nil {
		t.Fatal("第二个 tag 失败应报错")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
		t.Errorf("应保留 APIError 供调用方判型: %v", err)
	}
	if !strings.Contains(err.Error(), "second") || !strings.Contains(err.Error(), "first") {
		t.Errorf("错误应指出失败的 tag 与已完成的部分: %v", err)
	}
}

func TestValidateTags(t *testing.T) {
	c := &Client{}
	for _, ok := range []string{"managed-by:demo-nms", "PROD", "a-b_c:1", strings.Repeat("a", 255)} {
		if err := c.ValidateTags([]string{ok}); err != nil {
			t.Errorf("%q 应合法: %v", ok, err)
		}
	}
	if err := c.ValidateTags(nil); err != nil {
		t.Errorf("空列表应合法: %v", err)
	}
	for _, bad := range []string{"", "演示环境", "a b", "a,b", "a/b", "a?b", strings.Repeat("a", 256)} {
		err := c.ValidateTags([]string{"fine", bad})
		if err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("%q 应被拒并指名: %v", bad, err)
		}
	}
}

// 名字不合法时必须在发任何请求之前就拦下——否则 dry-run 之后的真实执行才会暴露。
func TestTagInvalidNameSendsNothing(t *testing.T) {
	c, a := newTagClient(t)
	err := c.Tag(context.Background(), "7", []string{"演示环境"}, nil)
	if err == nil || !strings.Contains(err.Error(), "演示环境") {
		t.Fatalf("中文 tag 名应本地报错: %v", err)
	}
	if a.hits != 0 {
		t.Errorf("不应发出任何请求，实际 %d 个", a.hits)
	}
}

// 批操作对同一账号的多台并发追加同一个新 tag：首建只能发一次，否则互相冲突。
func TestEnsureTagConcurrentCreatesOnce(t *testing.T) {
	_, a := newTagClient(t)
	c := newTestClient(t, a.handler())
	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = c.Tag(context.Background(), strconv.Itoa(i+1), []string{"shared"}, nil)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 台失败: %v", i+1, err)
		}
	}
	if len(a.created) != 1 {
		t.Errorf("并发首建应只发一次 POST /tags，实际 %d 次: %v", len(a.created), a.created)
	}
	if len(a.assigned) != len(errs) {
		t.Errorf("每台都应挂上: %v", a.assigned)
	}
}
