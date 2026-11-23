package digitalocean

// tag.go：provider.Tagger 的 DigitalOcean 实现。DO 的 tag 是账号级对象，须先存在
// 才能挂到资源上；挂/摘走 /v2/tags/{tag}/resources（官方 OpenAPI：tags_assign_resources
// 与 tags_unassign_resources，成功均为 204）。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"

	"github.com/hoshinojian/vpsctl/internal/provider"
)

// tagPattern 是 DO 对 tag 名的约束：字母、数字、下划线、短横线、冒号，最长 255
// （官方 OpenAPI tags.yml 的 name.pattern）。中文、空格等一律不收。
var tagPattern = regexp.MustCompile(`^[a-zA-Z0-9_\-:]{1,255}$`)

var _ provider.Tagger = (*Client)(nil) // Tag 能力编译期自证

func (c *Client) ValidateTags(tags []string) error {
	for _, t := range tags {
		if !tagPattern.MatchString(t) {
			return fmt.Errorf("tag 名 %q 不合法：只能含字母、数字、下划线、短横线、冒号，最长 255 个字符", t)
		}
	}
	return nil
}

// Tag 给节点追加 add、摘除 remove。名字先全部校验——不合规则一个请求都不发；
// 多 tag 中途失败时错误带上已完成的部分，重跑同一条命令即可补齐（幂等）。
func (c *Client) Tag(ctx context.Context, id string, add, remove []string) error {
	if err := c.ValidateTags(append(append([]string{}, add...), remove...)); err != nil {
		return err
	}
	var done []string
	for _, t := range add {
		if err := c.ensureTag(ctx, t); err != nil {
			return tagStepErr("创建 tag "+t, done, err)
		}
		if err := c.tagResource(ctx, http.MethodPost, t, id); err != nil {
			return tagStepErr("追加 tag "+t, done, err)
		}
		done = append(done, "+"+t)
	}
	for _, t := range remove {
		err := c.tagResource(ctx, http.MethodDelete, t, id)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			err = nil // tag 已不存在 = 早已摘除
		}
		if err != nil {
			return tagStepErr("摘除 tag "+t, done, err)
		}
		done = append(done, "-"+t)
	}
	return nil
}

// tagStepErr 把中途失败包装成带进度的话；%w 保留 APIError 供调用方判型。
func tagStepErr(step string, done []string, err error) error {
	if len(done) == 0 {
		return fmt.Errorf("%s: %w", step, err)
	}
	return fmt.Errorf("%s（此前已完成 %v，重跑可补齐）: %w", step, done, err)
}

// ensureTag 确认 tag 在账号里存在，不存在则创建。账号内串行：批操作并发挂同一个
// 新 tag 时只会有一次首建，其余等锁后直接命中缓存。
func (c *Client) ensureTag(ctx context.Context, name string) error {
	c.tagMu.Lock()
	defer c.tagMu.Unlock()
	if c.knownTags[name] {
		return nil
	}
	var pg tagPage
	err := c.do(ctx, http.MethodGet, "/tags/"+url.PathEscape(name), nil, &pg)
	var apiErr *APIError
	switch {
	case err == nil:
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
		if err := c.do(ctx, http.MethodPost, "/tags", tagName{Name: name}, &pg); err != nil {
			return err
		}
	default:
		return err
	}
	if c.knownTags == nil {
		c.knownTags = map[string]bool{}
	}
	c.knownTags[name] = true
	return nil
}

// tagResource 把 tag 挂到（POST）或摘自（DELETE）节点。
func (c *Client) tagResource(ctx context.Context, method, tag, id string) error {
	body := tagResourcesBody{Resources: []tagResource{{ResourceID: id, ResourceType: "droplet"}}}
	return c.do(ctx, method, "/tags/"+url.PathEscape(tag)+"/resources", body, nil)
}
