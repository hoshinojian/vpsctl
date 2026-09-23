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
// 两个必须的形态（P7 实证 + 真机复现 2026-09-23）：
//  1. Ubuntu 24.04 的 sshd 由 socket activation 拉起，sshd_config 的 Port 指令被忽略
//     直到禁用 ssh.socket；故先 disable ssh.socket 再经 drop-in 设 Port。
//  2. 注入体必须是 **ASCII-only**（DO user-data 非 ASCII 会被搅成 C1 控制字符，P75）
//     —— 注释也一律英文，禁 em dash 等字符。全链 ASCII 校验只在 --user-data 外部
//     文件路径上有（checkASCII），本函数生成物由下方 assertASCII 自守。
//
// 用写文件的方式（write_files 落 drop-in + runcmd 重启）而非内联 printf，避免
// YAML 流式标量里的反斜杠转义歧义（真机首跑实录：list 式 printf 的 \n 未解释）。
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
	if sshPort != 0 && sshPort != 22 {
		// write_files 用带引号的块标量（|），内容按行原样落盘，无转义歧义。
		b.WriteString("write_files:\n")
		b.WriteString("  - path: /etc/ssh/sshd_config.d/99-vpsctl.conf\n")
		b.WriteString("    permissions: '0644'\n")
		b.WriteString("    content: |\n")
		fmt.Fprintf(&b, "      Port %d\n", sshPort)
		b.WriteString("      PasswordAuthentication yes\n")
	}
	b.WriteString("ssh_pwauth: true\n")
	b.WriteString("chpasswd:\n")
	b.WriteString("  expire: false\n")
	b.WriteString("  users:\n")
	fmt.Fprintf(&b, "    - name: %s\n", user)
	// YAML 单引号风格：' → ''，其余字符（含反斜杠）原样
	fmt.Fprintf(&b, "      password: '%s'\n", strings.ReplaceAll(password, "'", "''"))
	b.WriteString("      type: text\n")
	if sshPort != 0 && sshPort != 22 {
		b.WriteString("runcmd:\n")
		// ASCII only: never use em dash or any non-ASCII byte here (P75).
		b.WriteString("  - [ systemctl, disable, --now, ssh.socket ]\n")
		b.WriteString("  - [ systemctl, enable, ssh.service ]\n")
		b.WriteString("  - [ systemctl, restart, ssh.service ]\n")
	}
	out := b.String()
	if err := assertASCII(out); err != nil {
		return "", err
	}
	return out, nil
}

// assertASCII 拒绝任何非 ASCII 字节（含 >0x7f）。DO user-data 非 ASCII 会被二次
// 编码损坏（P75），本函数生成物必须自守——全链 checkASCII 只覆盖外部 --user-data。
func assertASCII(s string) error {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7f {
			return fmt.Errorf("fleet: 生成的 cloud-init 含非 ASCII 字节（偏移 %d, 0x%02x）——DO user-data 需纯 ASCII（P75）", i, s[i])
		}
	}
	return nil
}
