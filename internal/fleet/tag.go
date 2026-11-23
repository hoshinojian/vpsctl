package fleet

// tag.go：给既有节点增删 tag 的批操作（不动电源、不重建）。与生命周期批操作一样
// 由 webui 与 CLI 共用。幂等：追加已持有的、摘除未持有的不进计划，重跑零写请求。

import (
	"context"
	"fmt"
	"sync"

	"github.com/hoshinojian/vpsctl/internal/provider"
)

// TagChange 是一台节点的 tag 变更（已剔除无变化项）。
type TagChange struct {
	Target
	Name   string   `json:"name,omitempty"`
	Before []string `json:"before,omitempty"` // 变更前的现有 tag（展示用）
	Add    []string `json:"add,omitempty"`
	Remove []string `json:"remove,omitempty"`
}

// PlanTags 按节点当前 tag 算出逐台变更，序 = servers 序；两侧都无变化的节点不进计划。
func PlanTags(servers []provider.Server, add, remove []string) []TagChange {
	var out []TagChange
	for _, s := range servers {
		ch := TagChange{Target: Target{Account: s.Account, ID: s.ID}, Name: s.Name, Before: s.Tags}
		for _, t := range add {
			if !hasString(s.Tags, t) {
				ch.Add = append(ch.Add, t)
			}
		}
		for _, t := range remove {
			if hasString(s.Tags, t) {
				ch.Remove = append(ch.Remove, t)
			}
		}
		if len(ch.Add) > 0 || len(ch.Remove) > 0 {
			out = append(out, ch)
		}
	}
	return out
}

// CheckTags 动手前按涉及账号整体预检：提供商须支持改 tag（provider.Tagger），且 tag 名
// 合其约束。任一不过即返回错误——上层据此整体中止，不产生任何写请求。
func CheckTags(clients []AccountClient, changes []TagChange, tags []string) error {
	byAcct := clientByID(clients)
	seen := make(map[string]bool, len(changes))
	for _, ch := range changes {
		if seen[ch.Account] {
			continue
		}
		seen[ch.Account] = true
		p, ok := byAcct[ch.Account]
		if !ok {
			return fmt.Errorf("账号 %q 无对应客户端", ch.Account)
		}
		tg, ok := p.(provider.Tagger)
		if !ok {
			return fmt.Errorf("账号 %s 的提供商不支持改 tag", ch.Account)
		}
		if err := tg.ValidateTags(tags); err != nil {
			return fmt.Errorf("账号 %s: %w", ch.Account, err)
		}
	}
	return nil
}

// TagBatch 并发执行逐台变更；未知账号、提供商未实现 provider.Tagger 记为该台失败。
func TagBatch(ctx context.Context, clients []AccountClient, changes []TagChange) []OpResult {
	byAcct := clientByID(clients)
	res := make([]OpResult, len(changes))
	var wg sync.WaitGroup
	for i, ch := range changes {
		wg.Add(1)
		go func(i int, ch TagChange) {
			defer wg.Done()
			p, ok := byAcct[ch.Account]
			if !ok {
				res[i] = OpResult{Account: ch.Account, ID: ch.ID, Error: "未知账号"}
				return
			}
			tg, ok := p.(provider.Tagger)
			if !ok {
				res[i] = OpResult{Account: ch.Account, ID: ch.ID, Error: "提供商不支持改 tag"}
				return
			}
			if err := tg.Tag(ctx, ch.ID, ch.Add, ch.Remove); err != nil {
				res[i] = OpResult{Account: ch.Account, ID: ch.ID, Error: err.Error()}
				return
			}
			res[i] = OpResult{Account: ch.Account, ID: ch.ID, OK: true}
		}(i, ch)
	}
	wg.Wait()
	return res
}

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
