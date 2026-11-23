package main

// tag_test.go：tag 子命令的参数防线与目标解析——参数错误须在读账号配置、发任何请求
// 之前就拦下；resolveServers 保留节点字段且顺序跟 --ids。

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hoshinojian/vpsctl/internal/fleet"
	"github.com/hoshinojian/vpsctl/internal/provider"
)

// listStub 只实现 List：resolveServers 单测只会调它，其余方法留 nil（误调即 panic）。
type listStub struct {
	provider.Provider
	servers []provider.Server
	err     error
}

func (s listStub) List(context.Context) ([]provider.Server, error) { return s.servers, s.err }

func TestRunTagRejectsBadArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"缺操作", nil, "add|remove"},
		{"未知操作", []string{"set"}, "set"},
		{"缺 --tags", []string{"add"}, "--tags"},
		{"--tags 只有分隔符", []string{"remove", "--tags", " , "}, "--tags"},
		{"--tag 与 --ids 同给", []string{"add", "--tags", "x", "--tag", "a", "--ids", "a1/1"}, "只能给一个"},
		{"未选目标", []string{"add", "--tags", "x"}, "需要 --tag 或 --ids"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runTag(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("runTag(%v) = %v, want 含 %q", tc.args, err, tc.want)
			}
		})
	}
}

func TestParseTagNames(t *testing.T) {
	got, err := parseTagNames(" a, b ,a,,c ")
	if err != nil || !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("parseTagNames = %v, %v; want [a b c]（去重保序）", got, err)
	}
	if _, err := parseTagNames(" , "); err == nil || !strings.Contains(err.Error(), "--tags") {
		t.Errorf("没有有效 tag 名应报错: %v", err)
	}
}

func TestResolveServers(t *testing.T) {
	servers := []provider.Server{
		{ID: "1", Account: "a1", Name: "n1", Tags: []string{"env:lab"}},
		{ID: "2", Account: "a1", Name: "n2"},
		{ID: "3", Account: "a2", Name: "n3", Tags: []string{"env:lab"}},
	}
	clients := []fleet.AccountClient{
		{Name: "a1", Provider: listStub{servers: servers[:2]}},
		{Name: "a2", Provider: listStub{servers: servers[2:]}},
	}
	ctx := context.Background()

	got, err := resolveServers(ctx, clients, "", "a2/3,a1/1")
	if err != nil || !reflect.DeepEqual(got, []provider.Server{servers[2], servers[0]}) {
		t.Errorf("按 --ids 选目标应保序并带节点字段: %+v err=%v", got, err)
	}
	if got, err = resolveServers(ctx, clients, "env:lab", ""); err != nil || !reflect.DeepEqual(got, []provider.Server{servers[0], servers[2]}) {
		t.Errorf("按 --tag 选目标: %+v err=%v", got, err)
	}
	if _, err = resolveServers(ctx, clients, "", "a1/99"); err == nil || !strings.Contains(err.Error(), "a1/99") {
		t.Errorf("不存在的目标应报错: %v", err)
	}
	if _, err = resolveServers(ctx, clients, "env:lab", "a1/1"); err == nil {
		t.Error("--tag 与 --ids 同给应报错")
	}

	// 任一账号查询失败即整体中止（宁可不动手）
	clients[1].Provider = listStub{err: errors.New("boom")}
	if _, err = resolveServers(ctx, clients, "", "a1/1"); err == nil ||
		!strings.Contains(err.Error(), "a2") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("账号查询失败应中止并指名: %v", err)
	}

	// 既有调用方只取账号 + ID，行为不变
	targets, err := resolveTargets(ctx, []fleet.AccountClient{{Name: "a1", Provider: listStub{servers: servers[:2]}}}, "", "a1/2")
	if err != nil || !reflect.DeepEqual(targets, []fleet.Target{{Account: "a1", ID: "2"}}) {
		t.Errorf("resolveTargets = %+v err=%v", targets, err)
	}
}
