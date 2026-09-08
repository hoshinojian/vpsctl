// lifecycle.go：跨账号生命周期批操作（关机/开机/删除）。
// webui 与 CLI 共用同一套实现——"宁可漏删不可误删"的语义只活在这里一份。
package fleet

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
)

// Target 是一台操作目标（账号 + 提供商 ID）。
type Target struct {
	Account string `json:"account"`
	ID      string `json:"id"`
}

// OpResult 是单台操作结果。
type OpResult struct {
	Account string `json:"account"`
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// SelectByTag 从节点清单里选出含指定 tag 的目标。
func SelectByTag(servers []provider.Server, tag string) []Target {
	var out []Target
	for _, s := range servers {
		for _, t := range s.Tags {
			if t == tag {
				out = append(out, Target{Account: s.Account, ID: s.ID})
				break
			}
		}
	}
	return out
}

// SelectByIDs 按 "account/id" 精确选择目标；任一不存在即报错（宁可不动手）。
func SelectByIDs(servers []provider.Server, ids []string) ([]Target, error) {
	known := make(map[string]Target, len(servers))
	for _, s := range servers {
		known[s.Account+"/"+s.ID] = Target{Account: s.Account, ID: s.ID}
	}
	out := make([]Target, 0, len(ids))
	for _, key := range ids {
		t, ok := known[key]
		if !ok {
			return nil, fmt.Errorf("目标 %q 不存在（格式应为 account/id，可用 list 查询）", key)
		}
		out = append(out, t)
	}
	return out, nil
}

func clientByID(clients []AccountClient) map[string]provider.Provider {
	m := make(map[string]provider.Provider, len(clients))
	for _, c := range clients {
		m[c.Name] = c.Provider
	}
	return m
}

// PowerBatch 并发对多台执行电源动作；未知账号记为该台失败。
func PowerBatch(ctx context.Context, clients []AccountClient, targets []Target, action string) []OpResult {
	byAcct := clientByID(clients)
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
			if _, err := p.Power(ctx, t.ID, action); err != nil {
				res[i] = OpResult{Account: t.Account, ID: t.ID, Error: err.Error()}
				return
			}
			res[i] = OpResult{Account: t.Account, ID: t.ID, OK: true}
		}(i, t)
	}
	wg.Wait()
	return res
}

// DeleteOptions 描述批量删除参数。
type DeleteOptions struct {
	ShutdownFirst bool          // 先优雅关机，未完成不删
	Wait          time.Duration // 关机完成等待上限（默认 120s）
	Poll          time.Duration // 轮询间隔（默认 2s）
}

func (o DeleteOptions) wait() time.Duration {
	if o.Wait <= 0 {
		return 120 * time.Second
	}
	return o.Wait
}

func (o DeleteOptions) poll() time.Duration {
	if o.Poll <= 0 {
		return 2 * time.Second
	}
	return o.Poll
}

// DeleteBatch 并发删除多台；ShutdownFirst 时先优雅关机并等待，
// 未在时限内完成则该台不删（宁可漏删，不可误删）。
func DeleteBatch(ctx context.Context, clients []AccountClient, targets []Target, o DeleteOptions) []OpResult {
	byAcct := clientByID(clients)
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
			if o.ShutdownFirst {
				if err := gracefulShutdownWait(ctx, p, t.ID, o.wait(), o.poll()); err != nil {
					res[i] = OpResult{Account: t.Account, ID: t.ID, Error: err.Error()}
					return
				}
			}
			if err := p.Delete(ctx, t.ID); err != nil {
				res[i] = OpResult{Account: t.Account, ID: t.ID, Error: "删除失败: " + err.Error()}
				return
			}
			res[i] = OpResult{Account: t.Account, ID: t.ID, OK: true}
		}(i, t)
	}
	wg.Wait()
	return res
}

// gracefulShutdownWait 发起优雅关机并轮询至完成；任何失败都返回带"未删除"
// 后缀的错误，语义上保证调用方不会继续删除。
func gracefulShutdownWait(ctx context.Context, p provider.Provider, id string, wait, poll time.Duration) error {
	ref, err := p.Power(ctx, id, provider.Shutdown)
	if err != nil {
		return fmt.Errorf("发起关机失败，未删除: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		status, err := p.ActionStatus(ctx, ref)
		if err != nil {
			return fmt.Errorf("查询关机状态失败，未删除: %w", err)
		}
		switch status {
		case "completed":
			return nil
		case "errored":
			return fmt.Errorf("关机失败（provider 报 errored），未删除")
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("关机超时（>%v），未删除", wait)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("已取消，未删除")
		case <-time.After(poll):
		}
	}
}
