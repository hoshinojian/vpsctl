package fleet

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
)

func TestRebuildBatch(t *testing.T) {
	fp := &fakeProvider{}
	clients := []AccountClient{{Name: "a1", Provider: fp}}
	res := RebuildBatch(context.Background(), clients, tgt("a1/7", "a1/8"), "ubuntu-24-04-x64", 0, 0)
	if len(res) != 2 || !res[0].OK || !res[1].OK {
		t.Fatalf("res = %+v", res)
	}
	if len(fp.rebuilds) != 2 {
		t.Fatalf("rebuilds = %v", fp.rebuilds)
	}
	for _, r := range fp.rebuilds {
		if !strings.HasSuffix(r, "@ubuntu-24-04-x64") {
			t.Errorf("镜像未传入: %v", fp.rebuilds)
		}
	}
	// 未知账号
	res = RebuildBatch(context.Background(), clients, tgt("nope/9"), "img", 0, 0)
	if res[0].OK || !strings.Contains(res[0].Error, "未知账号") {
		t.Errorf("未知账号应失败: %+v", res[0])
	}
}

func TestResizeBatch(t *testing.T) {
	t.Run("关机→改配→开机链", func(t *testing.T) {
		fp := &fakeProvider{
			getSeq: map[string][]provider.Server{
				"7": {provider.Server{ID: "7", Status: "active"}},
			},
			actionStatus: []string{"completed", "completed", "completed"},
		}
		clients := []AccountClient{{Name: "a1", Provider: fp}}
		res := ResizeBatch(context.Background(), clients, tgt("a1/7"), "s-2vcpu-2gb", false, 0, 0)
		if !res[0].OK {
			t.Fatalf("res = %+v err=%s", res[0], res[0].Error)
		}
		want := []string{"7:power_off", "7:power_on"}
		if len(fp.powerCalls) != 2 || fp.powerCalls[0] != want[0] || fp.powerCalls[1] != want[1] {
			t.Errorf("电源调用链 = %v", fp.powerCalls)
		}
		if len(fp.resizes) != 1 || fp.resizes[0] != "7@s-2vcpu-2gb disk=false" {
			t.Errorf("resizes = %v", fp.resizes)
		}
	})
	t.Run("已关机则跳过关机步骤", func(t *testing.T) {
		fp := &fakeProvider{
			getSeq:       map[string][]provider.Server{"7": {provider.Server{ID: "7", Status: "off"}}},
			actionStatus: []string{"completed"},
		}
		clients := []AccountClient{{Name: "a1", Provider: fp}}
		res := ResizeBatch(context.Background(), clients, tgt("a1/7"), "s-2vcpu-2gb", true, 0, 0)
		if !res[0].OK || len(fp.powerCalls) != 1 || fp.powerCalls[0] != "7:power_on" {
			t.Errorf("res=%+v power=%v", res, fp.powerCalls)
		}
		if fp.resizes[0] != "7@s-2vcpu-2gb disk=true" {
			t.Errorf("resizes = %v", fp.resizes)
		}
	})
	t.Run("关机失败不改配", func(t *testing.T) {
		fp := &fakeProvider{
			getSeq:       map[string][]provider.Server{"7": {provider.Server{ID: "7", Status: "active"}}},
			actionStatus: []string{"errored"},
		}
		clients := []AccountClient{{Name: "a1", Provider: fp}}
		res := ResizeBatch(context.Background(), clients, tgt("a1/7"), "s-2vcpu-2gb", false, time.Second, time.Millisecond)
		if res[0].OK || !strings.Contains(res[0].Error, "关机") || len(fp.resizes) != 0 {
			t.Errorf("关机失败应中止: %+v resizes=%v", res[0], fp.resizes)
		}
	})
}
