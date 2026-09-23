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
	return CloudInit(user, password, 0)
}

// CloudInit 在密码 cloud-config 之上，可选注入 sshd 高位端口。
//
// sshPort<=0 或 ==22 时输出与 CloudInitPassword 完全一致（不碰 sshd，保持缺省行为）；
// 非缺省端口时追加 sshd 配置段——这是把「建机即带高位口」收进 vpsctl 的关键：
// 原先该能力只活在外部编排仓（nmsctl）的 user-data 模板里。
//
// 两个必须的形态（P7 实证）：Ubuntu 24.04 的 sshd 由 socket activation 拉起，
// sshd_config 的 Port 指令被忽略——必须先 disable ssh.socket 再经 drop-in 设 Port，
// 否则机器固执地只监听 22。
func CloudInit(user, password string, sshPort int) (string, error) {
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
	if sshPort != 0 && (sshPort < 1 || sshPort > 65535) {
		return "", fmt.Errorf("fleet: ssh_port %d 越界（1-65535）", sshPort)
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
	if sshPort != 0 && sshPort != 22 {
		b.WriteString("\n")
		b.WriteString("runcmd:\n")
		b.WriteString("  # Ubuntu 24.04 socket activation ignores Port in sshd_config — disable\n")
		b.WriteString("  # ssh.socket, run ssh.service, then bind the drill port via drop-in.\n")
		b.WriteString("  - [ systemctl, disable, --now, ssh.socket ]\n")
		b.WriteString("  - [ systemctl, enable, ssh.service ]\n")
		fmt.Fprintf(&b, "  - [ sh, -c, 'printf \"Port %d\\nPasswordAuthentication yes\\n\" > /etc/ssh/sshd_config.d/99-vpsctl.conf' ]\n", sshPort)
		b.WriteString("  - [ systemctl, restart, ssh.service ]\n")
	}
	return b.String(), nil
}
