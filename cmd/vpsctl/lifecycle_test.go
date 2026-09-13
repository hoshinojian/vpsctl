package main

// delete --wait-gone 单测（自愈收口 E 项）：等待集只收删除结果 OK 的台——
// 失败台永不消失，纳入等待集只会白耗超时。

import (
	"testing"

	"github.com/hoshinojian/vpsctl/internal/fleet"
)

func TestOKTargetsOnlySuccessful(t *testing.T) {
	res := []fleet.OpResult{
		{Account: "a1", ID: "7", OK: true},
		{Account: "a1", ID: "8", OK: false, Error: "删除失败: 404"},
		{Account: "a2", ID: "9", OK: true},
	}
	got := okTargets(res)
	if len(got) != 2 {
		t.Fatalf("只应保留 OK 台: %+v", got)
	}
	if got[0] != (fleet.Target{Account: "a1", ID: "7"}) || got[1] != (fleet.Target{Account: "a2", ID: "9"}) {
		t.Errorf("等待集不符: %+v", got)
	}
	if got := okTargets([]fleet.OpResult{{Account: "a1", ID: "8", OK: false}}); len(got) != 0 {
		t.Errorf("全失败批次等待集应为空: %+v", got)
	}
	if got := okTargets(nil); len(got) != 0 {
		t.Errorf("空结果等待集应为空: %+v", got)
	}
}
