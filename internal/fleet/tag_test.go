package fleet

// tag_test.go：tag 变更计划与批执行——已满足的节点不进计划（幂等，重跑零写请求）、
// 提供商未实现 Tag 能力按台失败、动手前整体预检。

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/hoshinojian/vpsctl/internal/provider"
)

// fakeTagger 给 fakeProvider 补上 provider.Tagger，记录收到的变更。
type fakeTagger struct {
	*fakeProvider
	mu       sync.Mutex
	got      []TagChange
	validate func([]string) error
	tagErr   error
}

func newFakeTagger() *fakeTagger { return &fakeTagger{fakeProvider: &fakeProvider{}} }

func (f *fakeTagger) ValidateTags(tags []string) error {
	if f.validate == nil {
		return nil
	}
	return f.validate(tags)
}

func (f *fakeTagger) Tag(_ context.Context, id string, add, remove []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, TagChange{Target: Target{ID: id}, Add: add, Remove: remove})
	return f.tagErr
}

func TestPlanTags(t *testing.T) {
	servers := []provider.Server{
		{ID: "1", Account: "a1", Name: "n1", Tags: []string{"keep", "old"}},
		{ID: "2", Account: "a1", Name: "n2"},
		{ID: "3", Account: "a2", Name: "n3", Tags: []string{"new"}},
		{ID: "4", Account: "a2", Name: "n4", Tags: []string{"new", "keep"}}, // 已全满足，不进计划
	}
	got := PlanTags(servers, []string{"new", "keep"}, []string{"old"})
	want := []TagChange{
		// n1 已有 keep：只补 new、摘 old
		{Target: Target{Account: "a1", ID: "1"}, Name: "n1", Before: []string{"keep", "old"}, Add: []string{"new"}, Remove: []string{"old"}},
		// n2 一无所有：old 未持有，摘除不进计划
		{Target: Target{Account: "a1", ID: "2"}, Name: "n2", Add: []string{"new", "keep"}},
		// n3 已有 new：只补 keep
		{Target: Target{Account: "a2", ID: "3"}, Name: "n3", Before: []string{"new"}, Add: []string{"keep"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PlanTags = %+v\nwant %+v", got, want)
	}
}

func TestPlanTagsAllSatisfiedIsEmpty(t *testing.T) {
	servers := []provider.Server{{ID: "1", Account: "a1", Tags: []string{"x"}}}
	if got := PlanTags(servers, []string{"x"}, []string{"y"}); len(got) != 0 {
		t.Errorf("已满足时应为空计划: %+v", got)
	}
}

func TestTagBatch(t *testing.T) {
	tg := newFakeTagger()
	clients := []AccountClient{
		{Name: "a1", Provider: tg},
		{Name: "a2", Provider: &fakeProvider{}}, // 未实现 provider.Tagger
	}
	changes := []TagChange{
		{Target: Target{Account: "a1", ID: "7"}, Add: []string{"x"}, Remove: []string{"y"}},
		{Target: Target{Account: "a2", ID: "8"}, Add: []string{"x"}},
		{Target: Target{Account: "nope", ID: "9"}, Add: []string{"x"}},
	}
	res := TagBatch(context.Background(), clients, changes)
	if len(res) != 3 {
		t.Fatalf("results = %+v", res)
	}
	if !res[0].OK {
		t.Errorf("支持 Tag 的账号应成功: %+v", res[0])
	}
	if res[1].OK || !strings.Contains(res[1].Error, "不支持") {
		t.Errorf("提供商不支持应按台失败: %+v", res[1])
	}
	if res[2].OK || !strings.Contains(res[2].Error, "未知账号") {
		t.Errorf("未知账号应失败: %+v", res[2])
	}
	if len(tg.got) != 1 || tg.got[0].ID != "7" ||
		!reflect.DeepEqual(tg.got[0].Add, []string{"x"}) || !reflect.DeepEqual(tg.got[0].Remove, []string{"y"}) {
		t.Errorf("只有 a1/7 应下发且变更原样: %+v", tg.got)
	}
}

func TestTagBatchPropagatesError(t *testing.T) {
	tg := newFakeTagger()
	tg.tagErr = errors.New("boom")
	res := TagBatch(context.Background(), []AccountClient{{Name: "a1", Provider: tg}},
		[]TagChange{{Target: Target{Account: "a1", ID: "7"}, Add: []string{"x"}}})
	if len(res) != 1 || res[0].OK || res[0].Error != "boom" {
		t.Errorf("提供商错误应原样进结果: %+v", res)
	}
}

func TestCheckTags(t *testing.T) {
	changes := []TagChange{
		{Target: Target{Account: "a1", ID: "7"}},
		{Target: Target{Account: "a2", ID: "8"}},
	}
	both := func(a1, a2 provider.Provider) []AccountClient {
		return []AccountClient{{Name: "a1", Provider: a1}, {Name: "a2", Provider: a2}}
	}

	if err := CheckTags(both(newFakeTagger(), newFakeTagger()), changes, []string{"x"}); err != nil {
		t.Errorf("全部支持且名字合法应通过: %v", err)
	}

	err := CheckTags(both(newFakeTagger(), &fakeProvider{}), changes, []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "a2") || !strings.Contains(err.Error(), "不支持") {
		t.Errorf("有账号的提供商不支持应整体拒绝并指名: %v", err)
	}

	bad := newFakeTagger()
	bad.validate = func([]string) error { return errors.New(`tag 名 "演示环境" 不合法`) }
	err = CheckTags(both(bad, newFakeTagger()), changes, []string{"演示环境"})
	if err == nil || !strings.Contains(err.Error(), "a1") || !strings.Contains(err.Error(), "演示环境") {
		t.Errorf("名字不合提供商约束应整体拒绝并指名: %v", err)
	}

	err = CheckTags([]AccountClient{{Name: "a1", Provider: newFakeTagger()}}, changes, []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "a2") {
		t.Errorf("账号无客户端应拒绝: %v", err)
	}
}
