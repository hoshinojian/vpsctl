// lifecycle.go：跨账号生命周期批操作（关机/开机/删除）。
// webui 与 CLI 共用同一套实现——"宁可漏删不可误删"的语义只活在这里一份。
package fleet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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

// waitGoneMinPoll 是等待消失的轮询间隔下限：DO 删除为异步、list 最终一致，
// 间隔太短会把一致性抖动误判成超时。
const waitGoneMinPoll = 5 * time.Second

// WaitGoneOptions 描述"等待删除目标从 list 消失"的参数。
type WaitGoneOptions struct {
	Timeout time.Duration // 总等待上限（必填 >0）
	Poll    time.Duration // 轮询间隔，低于 5s 收敛到 5s（DO 最终一致性）
	// Now/Sleep 供测试注入假时钟；nil 用真实时间。
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// WaitGone 删除发起后轮询各账号 Provider.List（按账号过滤，每账号每轮一次），
// 直到全部目标从所属账号的节点清单消失。只应传入删除结果 OK 的台——失败台
// 永不消失。瞬时 List 失败（DO 抖动）不算失败、继续轮询，仅记入超时信息。
// 超时返回错误并列出未消失目标（account/id）；ctx 取消立即返回。
func WaitGone(ctx context.Context, clients []AccountClient, targets []Target, o WaitGoneOptions) error {
	if len(targets) == 0 {
		return nil
	}
	if o.Timeout <= 0 {
		return errors.New("fleet: WaitGone 需要 Timeout > 0")
	}
	pending := map[string]map[string]bool{} // account -> 未消失 id 集
	accounts := []string{}                  // 保持稳定遍历序
	for _, t := range targets {
		if pending[t.Account] == nil {
			pending[t.Account] = map[string]bool{}
			accounts = append(accounts, t.Account)
		}
		pending[t.Account][t.ID] = true
	}
	provs := map[string]provider.Provider{}
	for _, c := range clients {
		if _, need := pending[c.Name]; need {
			provs[c.Name] = c.Provider
		}
	}
	for _, acct := range accounts {
		if provs[acct] == nil {
			return fmt.Errorf("账号 %q 无对应客户端，无法核对是否消失", acct)
		}
	}
	poll := max(o.Poll, waitGoneMinPoll)
	now := o.Now
	if now == nil {
		now = time.Now
	}
	sleep := o.Sleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	deadline := now().Add(o.Timeout)
	var lastErr error
	for {
		remaining := 0
		for _, acct := range accounts {
			ids := pending[acct]
			if len(ids) == 0 {
				continue // 该账号已全部消失，不再查询
			}
			servers, err := provs[acct].List(ctx)
			if err != nil {
				lastErr = fmt.Errorf("账号 %s 查询节点失败: %w", acct, err)
				remaining += len(ids)
				continue
			}
			present := make(map[string]bool, len(servers))
			for _, s := range servers {
				present[s.ID] = true
			}
			for id := range ids {
				if !present[id] {
					delete(ids, id) // 清单里已不见 → 消失
				}
			}
			remaining += len(ids)
		}
		if remaining == 0 {
			return nil
		}
		if !now().Before(deadline) {
			msg := fmt.Sprintf("等待 %v 超时，仍有 %d 台未从 list 消失（DO 删除为异步，可稍后 list 复查或加长 --wait-gone）: %s",
				o.Timeout, remaining, joinPending(pending, accounts))
			if lastErr != nil {
				msg += fmt.Sprintf("（期间最近一次查询失败: %v）", lastErr)
			}
			return errors.New(msg)
		}
		if err := sleep(ctx, poll); err != nil {
			return fmt.Errorf("等待消失被取消: %w", err)
		}
	}
}

// joinPending 把未消失目标按账号序拼成 "account/id" 逗号串（错误信息用）。
func joinPending(pending map[string]map[string]bool, accounts []string) string {
	parts := make([]string, 0, len(accounts))
	for _, acct := range accounts {
		ids := make([]string, 0, len(pending[acct]))
		for id := range pending[acct] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			parts = append(parts, acct+"/"+id)
		}
	}
	return strings.Join(parts, ", ")
}
