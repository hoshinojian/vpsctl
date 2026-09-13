package fleet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
)

func tgt(keys ...string) []Target {
	out := make([]Target, 0, len(keys))
	for _, k := range keys {
		i := strings.Index(k, "/")
		out = append(out, Target{Account: k[:i], ID: k[i+1:]})
	}
	return out
}

func TestSelectByTag(t *testing.T) {
	servers := []provider.Server{
		{ID: "1", Account: "a1", Tags: []string{"batch:x"}},
		{ID: "2", Account: "a1", Tags: []string{"env:lab"}},
		{ID: "3", Account: "a2", Tags: []string{"batch:x", "env:lab"}},
	}
	got := SelectByTag(servers, "batch:x")
	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "3" {
		t.Errorf("SelectByTag = %+v", got)
	}
	if got := SelectByTag(servers, "nope"); len(got) != 0 {
		t.Errorf("无匹配应为空: %+v", got)
	}
}

func TestSelectByIDs(t *testing.T) {
	servers := []provider.Server{{ID: "1", Account: "a1"}, {ID: "2", Account: "a2"}}
	got, err := SelectByIDs(servers, []string{"a1/1", "a2/2"})
	if err != nil || len(got) != 2 || got[1].Account != "a2" {
		t.Errorf("SelectByIDs = %+v err=%v", got, err)
	}
	// account/id 不存在 → 报错并列出有效格式
	if _, err := SelectByIDs(servers, []string{"a1/99"}); err == nil || !strings.Contains(err.Error(), "a1/99") {
		t.Errorf("不存在的目标应报错: %v", err)
	}
	// 格式错误
	if _, err := SelectByIDs(servers, []string{"1"}); err == nil {
		t.Error("缺账号前缀应报错")
	}
}

func TestPowerBatch(t *testing.T) {
	fp := &fakeProvider{}
	clients := []AccountClient{{Name: "a1", Provider: fp}, {Name: "a2", Provider: &fakeProvider{}}}
	res := PowerBatch(context.Background(), clients, tgt("a1/7", "a1/8", "nope/9"), provider.PowerOff)
	if len(res) != 3 {
		t.Fatalf("results = %+v", res)
	}
	if !res[0].OK || !res[1].OK {
		t.Errorf("正常目标应成功: %+v", res)
	}
	if res[2].OK || !strings.Contains(res[2].Error, "未知账号") {
		t.Errorf("未知账号应失败: %+v", res[2])
	}
	got := append([]string(nil), fp.powerCalls...)
	if len(got) != 2 || got[0] != "7:power_off" || got[1] != "8:power_off" {
		// 并发提交顺序不定，排序比较
		if !(got[0] == "8:power_off" && got[1] == "7:power_off") {
			t.Errorf("powerCalls = %v", fp.powerCalls)
		}
	}
}

func TestDeleteBatch(t *testing.T) {
	t.Run("直接删除", func(t *testing.T) {
		fp := &fakeProvider{}
		clients := []AccountClient{{Name: "a1", Provider: fp}}
		res := DeleteBatch(context.Background(), clients, tgt("a1/7"),
			DeleteOptions{Wait: time.Second, Poll: time.Millisecond})
		if !res[0].OK || len(fp.deletes) != 1 || fp.powerCalls != nil {
			t.Errorf("res=%+v deletes=%v power=%v", res, fp.deletes, fp.powerCalls)
		}
	})
	t.Run("先关机完成再删", func(t *testing.T) {
		fp := &fakeProvider{actionStatus: []string{"in-progress", "completed"}}
		clients := []AccountClient{{Name: "a1", Provider: fp}}
		res := DeleteBatch(context.Background(), clients, tgt("a1/9"),
			DeleteOptions{ShutdownFirst: true, Wait: time.Second, Poll: time.Millisecond})
		if !res[0].OK || len(fp.deletes) != 1 || len(fp.powerCalls) != 1 {
			t.Errorf("res=%+v deletes=%v power=%v", res, fp.deletes, fp.powerCalls)
		}
	})
	t.Run("关机 errored 不删", func(t *testing.T) {
		fp := &fakeProvider{actionStatus: []string{"errored"}}
		clients := []AccountClient{{Name: "a1", Provider: fp}}
		res := DeleteBatch(context.Background(), clients, tgt("a1/9"),
			DeleteOptions{ShutdownFirst: true, Wait: time.Second, Poll: time.Millisecond})
		if res[0].OK || !strings.Contains(res[0].Error, "未删除") || len(fp.deletes) != 0 {
			t.Errorf("errored 应不删: %+v", res)
		}
	})
	t.Run("关机超时不删", func(t *testing.T) {
		fp := &fakeProvider{actionStatus: []string{"in-progress"}}
		clients := []AccountClient{{Name: "a1", Provider: fp}}
		res := DeleteBatch(context.Background(), clients, tgt("a1/9"),
			DeleteOptions{ShutdownFirst: true, Wait: 10 * time.Millisecond, Poll: time.Millisecond})
		if res[0].OK || !strings.Contains(res[0].Error, "未删除") || len(fp.deletes) != 0 {
			t.Errorf("超时应不删: %+v", res)
		}
	})
}

// ---- delete --wait-gone：删除后等待目标从 list 消失（自愈收口 E 项）----

// listReply 是 listSeqFake 单次 List 的编排回应。
type listReply struct {
	servers []provider.Server
	err     error
}

// listSeqFake 按序返回 List 结果的假 Provider（wait-gone 轮询用）；其余方法
// 不该被调用——内嵌 nil 接口兜底，误调即 panic。
type listSeqFake struct {
	provider.Provider
	mu    sync.Mutex
	calls int
	seq   []listReply // 依次返回；耗尽后停驻最后一个
}

func (f *listSeqFake) List(context.Context) ([]provider.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seq) == 0 {
		return nil, errors.New("listSeqFake: 未编排 List 序列")
	}
	i := f.calls
	if i >= len(f.seq)-1 {
		i = len(f.seq) - 1
	}
	f.calls++
	return f.seq[i].servers, f.seq[i].err
}

func (f *listSeqFake) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeWaitClock 假时钟：Sleep 记录每次间隔并推进假 now（WaitGone 注入用）。
type fakeWaitClock struct {
	now   time.Time
	slept []time.Duration
}

func (c *fakeWaitClock) Now() time.Time { return c.now }

func (c *fakeWaitClock) Sleep(_ context.Context, d time.Duration) error {
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	return nil
}

func waitClients() ([]AccountClient, *listSeqFake, *listSeqFake) {
	c1 := &listSeqFake{seq: []listReply{
		{servers: []provider.Server{{ID: "7", Account: "a1"}, {ID: "8", Account: "a1"}}},
		{servers: []provider.Server{{ID: "8", Account: "a1"}}}, // 第 2 轮 7 消失
		{servers: nil}, // 第 3 轮 8 消失
	}}
	c2 := &listSeqFake{seq: []listReply{
		{servers: []provider.Server{{ID: "9", Account: "a2"}}},
		{servers: nil}, // 第 2 轮 9 消失
	}}
	return []AccountClient{{Name: "a1", Provider: c1}, {Name: "a2", Provider: c2}}, c1, c2
}

// 全部消失 → nil；每账号独立 List（按 account 过滤），逐台逐步消失。
func TestWaitGoneSuccess(t *testing.T) {
	clients, c1, c2 := waitClients()
	ck := &fakeWaitClock{now: time.Unix(0, 0)}
	err := WaitGone(context.Background(), clients, tgt("a1/7", "a1/8", "a2/9"),
		WaitGoneOptions{Timeout: time.Minute, Now: ck.Now, Sleep: ck.Sleep})
	if err != nil {
		t.Fatalf("应等到全部消失: %v", err)
	}
	if c1.listCalls() != 3 || c2.listCalls() != 2 {
		t.Errorf("各账号应按各自节奏轮询: a1=%d a2=%d", c1.listCalls(), c2.listCalls())
	}
	// a2 第 2 轮已清空，第 3 轮不再查询；共睡 2 轮
	if len(ck.slept) != 2 {
		t.Errorf("slept=%v", ck.slept)
	}
}

// 初始即不存在 → 一次 List 确认即返回，零睡眠。
func TestWaitGoneInitiallyAbsent(t *testing.T) {
	c1 := &listSeqFake{seq: []listReply{{servers: nil}}}
	ck := &fakeWaitClock{now: time.Unix(0, 0)}
	err := WaitGone(context.Background(),
		[]AccountClient{{Name: "a1", Provider: c1}}, tgt("a1/7"),
		WaitGoneOptions{Timeout: time.Minute, Now: ck.Now, Sleep: ck.Sleep})
	if err != nil || c1.listCalls() != 1 || len(ck.slept) != 0 {
		t.Errorf("err=%v calls=%d slept=%v", err, c1.listCalls(), ck.slept)
	}
}

// 永不消失 → 超时错误，含全部未消失 account/id 清单。
func TestWaitGoneTimeoutListsPending(t *testing.T) {
	c1 := &listSeqFake{seq: []listReply{
		{servers: []provider.Server{{ID: "7", Account: "a1"}, {ID: "8", Account: "a1"}}}, // 永远都在
	}}
	ck := &fakeWaitClock{now: time.Unix(0, 0)}
	err := WaitGone(context.Background(),
		[]AccountClient{{Name: "a1", Provider: c1}}, tgt("a1/7", "a1/8"),
		WaitGoneOptions{Timeout: 12 * time.Second, Now: ck.Now, Sleep: ck.Sleep})
	if err == nil {
		t.Fatal("超时应报错")
	}
	for _, key := range []string{"超时", "a1/7", "a1/8"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("超时信息缺 %q: %v", key, err)
		}
	}
}

// 轮询间隔收敛到 ≥5s（DO 最终一致性，别把抖动当超时）：Poll 给 1ms 也必须睡满 5s。
func TestWaitGonePollClampedToMin(t *testing.T) {
	c1 := &listSeqFake{seq: []listReply{{servers: []provider.Server{{ID: "7", Account: "a1"}}}}}
	ck := &fakeWaitClock{now: time.Unix(0, 0)}
	_ = WaitGone(context.Background(),
		[]AccountClient{{Name: "a1", Provider: c1}}, tgt("a1/7"),
		WaitGoneOptions{Timeout: 12 * time.Second, Poll: time.Millisecond, Now: ck.Now, Sleep: ck.Sleep})
	if len(ck.slept) == 0 {
		t.Fatal("应有轮询间隔")
	}
	for i, d := range ck.slept {
		if d < 5*time.Second {
			t.Errorf("第 %d 次间隔 %v < 5s，最终一致性下会误判超时", i, d)
		}
	}
}

// ctx 取消（Ctrl-C）→ 立即返回取消错误，不再等满超时。
func TestWaitGoneContextCanceled(t *testing.T) {
	c1 := &listSeqFake{seq: []listReply{{servers: []provider.Server{{ID: "7", Account: "a1"}}}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WaitGone(ctx, []AccountClient{{Name: "a1", Provider: c1}}, tgt("a1/7"),
		WaitGoneOptions{Timeout: time.Hour})
	if err == nil || !strings.Contains(err.Error(), "取消") {
		t.Errorf("取消应立即报错并注明: %v", err)
	}
}

// 瞬时 List 抖动（DO 5xx）不算失败：错一次后消失 → 成功。
func TestWaitGoneToleratesTransientListError(t *testing.T) {
	c1 := &listSeqFake{seq: []listReply{
		{err: errors.New("503 transient")},
		{servers: nil},
	}}
	ck := &fakeWaitClock{now: time.Unix(0, 0)}
	err := WaitGone(context.Background(),
		[]AccountClient{{Name: "a1", Provider: c1}}, tgt("a1/7"),
		WaitGoneOptions{Timeout: time.Minute, Now: ck.Now, Sleep: ck.Sleep})
	if err != nil {
		t.Fatalf("瞬时抖动不应失败: %v", err)
	}
}

// 等待集出现无客户端的账号 → 立即报错（不发请求），绝不静默挂到超时。
func TestWaitGoneUnknownAccount(t *testing.T) {
	c1 := &listSeqFake{seq: []listReply{{servers: nil}}}
	err := WaitGone(context.Background(),
		[]AccountClient{{Name: "a1", Provider: c1}}, tgt("a1/7", "a9/1"),
		WaitGoneOptions{Timeout: time.Minute})
	if err == nil || !strings.Contains(err.Error(), "a9") {
		t.Errorf("未知账号应立即报错: %v", err)
	}
	if c1.listCalls() != 0 {
		t.Errorf("校验失败不应发请求: calls=%d", c1.listCalls())
	}
}

// 空等待集 → 直接 nil（全部删除失败的批次没有可等待的对象）。
func TestWaitGoneEmptyTargets(t *testing.T) {
	if err := WaitGone(context.Background(), nil, nil, WaitGoneOptions{Timeout: time.Minute}); err != nil {
		t.Errorf("空目标应直接返回: %v", err)
	}
}
