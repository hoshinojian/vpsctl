// Package config 加载 vpsctl 的多账号配置文件。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Account 是单个提供商账号的凭据描述。
type Account struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Token    string `json:"token"`
}

// File 是 accounts.json 的顶层结构。
type File struct {
	Accounts []Account `json:"accounts"`
}

// DefaultPath 解析配置文件路径：override > 环境变量 VPSCTL_ACCOUNTS >
// ~/.config/vpsctl/accounts.json。override 支持 ~/ 前缀。
func DefaultPath(override string) (string, error) {
	if override == "" {
		override = os.Getenv("VPSCTL_ACCOUNTS")
	}
	if override == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("定位用户主目录: %w", err)
		}
		return filepath.Join(home, ".config", "vpsctl", "accounts.json"), nil
	}
	return expandHome(override)
}

// Load 读取并校验配置；返回配置与告警列表（如 token 文件权限过宽）。
func Load(path string) (*File, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	if err := f.Validate(); err != nil {
		return nil, nil, fmt.Errorf("配置 %s 不合法: %w", path, err)
	}
	return &f, permWarnings(path), nil
}

// Validate 校验：accounts 非空、字段非空、账号名唯一。
func (f *File) Validate() error {
	if len(f.Accounts) == 0 {
		return errors.New("accounts 为空")
	}
	seen := make(map[string]bool, len(f.Accounts))
	for i, a := range f.Accounts {
		if a.Name == "" {
			return fmt.Errorf("accounts[%d].name 为空", i)
		}
		if seen[a.Name] {
			return fmt.Errorf("账号名 %q 重复", a.Name)
		}
		seen[a.Name] = true
		if a.Provider == "" {
			return fmt.Errorf("账号 %q 缺少 provider", a.Name)
		}
		if a.Token == "" {
			return fmt.Errorf("账号 %q 缺少 token", a.Name)
		}
	}
	return nil
}

func permWarnings(path string) []string {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return []string{fmt.Sprintf("配置文件 %s 权限为 %04o，含 API token，建议 chmod 600", path, perm)}
	}
	return nil
}

func expandHome(path string) (string, error) {
	if !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("展开 %s: %w", path, err)
	}
	return filepath.Join(home, path[2:]), nil
}
