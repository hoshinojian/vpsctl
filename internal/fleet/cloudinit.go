package fleet

import (
	"errors"
	"fmt"
	"strings"
)

// CloudInitPassword 生成设置用户密码并开启 SSH 密码登录的最小 cloud-config。
// 只覆盖密码一件事——与自定义 --user-data 互斥（零依赖没有 YAML 合并能力），
// 互斥校验由调用方（cmd create）负责。
func CloudInitPassword(user, password string) (string, error) {
	if user == "" {
		user = "root"
	}
	if password == "" {
		return "", errors.New("fleet: 密码为空")
	}
	if strings.ContainsAny(user, "\n\r\x00") {
		return "", errors.New("fleet: ssh_user 含换行/控制字符，无法安全注入 cloud-config")
	}
	if strings.ContainsAny(password, "\n\r\x00") {
		return "", errors.New("fleet: 密码含换行/控制字符，无法安全注入 cloud-config")
	}
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("ssh_pwauth: true\n")
	b.WriteString("chpasswd:\n")
	b.WriteString("  expire: false\n")
	b.WriteString("  users:\n")
	fmt.Fprintf(&b, "    - name: %s\n", user)
	// YAML 单引号风格：' → ''，其余字符（含反斜杠）原样
	fmt.Fprintf(&b, "      password: '%s'\n", strings.ReplaceAll(password, "'", "''"))
	b.WriteString("      type: text\n")
	return b.String(), nil
}
