package fleet

import (
	"context"
	"strings"
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
