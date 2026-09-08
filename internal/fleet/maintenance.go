package fleet

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
)

// maintenance.go：重装/改配批操作。与生命周期批操作一样由 webui 与 CLI 共用。

// ActionWait 描述异步动作的等待参数。
type ActionWait struct {
	Wait time.Duration // 完成等待上限（默认 120s）
	Poll time.Duration // 轮询间隔（默认 2s）
}

func (w ActionWait) wait() time.Duration {
	if w.Wait <= 0 {
		return 120 * time.Second
	}
	return w.Wait
}

func (w ActionWait) poll() time.Duration {
	if w.Poll <= 0 {
		return 2 * time.Second
	}
	return w.Poll
}

// waitActionCompleted 轮询动作至 completed；errored/超时/取消均报错。
func waitActionCompleted(ctx context.Context, p provider.Provider, ref provider.ActionRef, w ActionWait) error {
	deadline := time.Now().Add(w.wait())
	for {
		status, err := p.ActionStatus(ctx, ref)
		if err != nil {
			return err
		}
		switch status {
		case "completed":
			return nil
		case "errored":
			return fmt.Errorf("provider 报 errored")
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("等待超时（>%v）", w.wait())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(w.poll()):
		}
	}
}

// RebuildBatch 并发重装多台：单动作，轮询至完成。数据清空不可恢复，
// 调用方（CLI/UI）负责事前确认。
func RebuildBatch(ctx context.Context, clients []AccountClient, targets []Target, image string, wait, poll time.Duration) []OpResult {
	byAcct := clientByID(clients)
	w := ActionWait{Wait: wait, Poll: poll}
	res := make([]OpResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()
			p, ok := byAcct[t.Account]
			if !ok {
				res[i] = OpResult{Account: t.Account, ID: t.ID, Error: "未知账号"}
				return
			}
			ref, err := p.Rebuild(ctx, t.ID, image)
			if err != nil {
				res[i] = OpResult{Account: t.Account, ID: t.ID, Error: "发起重装失败: " + err.Error()}
				return
			}
			if err := waitActionCompleted(ctx, p, ref, w); err != nil {
				res[i] = OpResult{Account: t.Account, ID: t.ID,
					Error: fmt.Sprintf("重装未完成（节点已开始重装）: %v", err)}
				return
			}
			res[i] = OpResult{Account: t.Account, ID: t.ID, OK: true}
		}(i, t)
	}
	wg.Wait()
	return res
}

// ResizeBatch 并发改配：每台执行 关机（已 off 则跳过）→ resize → 开机，
// 任一步失败即中止该台后续步骤。resizeDisk=true 同时扩磁盘（不可逆）。
func ResizeBatch(ctx context.Context, clients []AccountClient, targets []Target, size string, resizeDisk bool, wait, poll time.Duration) []OpResult {
	byAcct := clientByID(clients)
	w := ActionWait{Wait: wait, Poll: poll}
	res := make([]OpResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()
			p, ok := byAcct[t.Account]
			if !ok {
				res[i] = OpResult{Account: t.Account, ID: t.ID, Error: "未知账号"}
				return
			}
			fail := func(format string, args ...any) {
				res[i] = OpResult{Account: t.Account, ID: t.ID, Error: fmt.Sprintf(format, args...)}
			}
			// 已关机则跳过关机步骤（DO 对已关机节点再发 power_off 会报错）
			sv, err := p.Get(ctx, t.ID)
			switch {
			case err != nil:
				fail("查询节点状态失败: %v", err)
				return
			case sv.Status != "off":
				ref, err := p.Power(ctx, t.ID, provider.PowerOff)
				if err != nil {
					fail("发起关机失败: %v", err)
					return
				}
				if err := waitActionCompleted(ctx, p, ref, w); err != nil {
					fail("关机未完成，已中止改配: %v", err)
					return
				}
			}
			ref, err := p.Resize(ctx, t.ID, size, resizeDisk)
			if err != nil {
				fail("发起改配失败: %v", err)
				return
			}
			if err := waitActionCompleted(ctx, p, ref, w); err != nil {
				fail("改配未完成: %v", err)
				return
			}
			if ref, err = p.Power(ctx, t.ID, provider.PowerOn); err != nil {
				fail("改配完成但开机失败（请手动开机）: %v", err)
				return
			}
			if err := waitActionCompleted(ctx, p, ref, w); err != nil {
				fail("改配完成但开机未完成（请检查节点状态）: %v", err)
				return
			}
			res[i] = OpResult{Account: t.Account, ID: t.ID, OK: true}
		}(i, t)
	}
	wg.Wait()
	return res
}
