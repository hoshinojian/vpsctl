package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/hoshinojian/vpsctl/internal/config"
	"github.com/hoshinojian/vpsctl/internal/fleet"
	"github.com/hoshinojian/vpsctl/internal/provider"
)

// loadAccounts 解析并加载账号配置，权限告警打到 stderr。
func loadAccounts(pathOverride string) (*config.File, error) {
	path, err := config.DefaultPath(pathOverride)
	if err != nil {
		return nil, err
	}
	f, warns, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "警告:", w)
	}
	return f, nil
}

// buildClients 为每个账号构造客户端。
func buildClients(cfg *config.File) ([]fleet.AccountClient, error) {
	clients := make([]fleet.AccountClient, 0, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		p, err := provider.New(a.Provider, a.Name, a.Token, nil)
		if err != nil {
			return nil, fmt.Errorf("账号 %s: %w", a.Name, err)
		}
		clients = append(clients, fleet.AccountClient{
			Name: a.Name, ProviderName: a.Provider, Provider: p,
		})
	}
	return clients, nil
}

// splitCSV 按逗号切分并去掉空片段。
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
