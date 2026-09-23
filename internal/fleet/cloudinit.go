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

// CloudInit 是 CloudInitSpec 的便捷包装（密码 + 可选高位口），保留既有调用面。
func CloudInit(user, password string, sshPort int) (string, error) {
	return CloudInitSpec(CloudInitParams{User: user, Password: password, SSHPort: sshPort})
}

// CloudInitParams 描述一台机器的 cloud-init 置备参数。
type CloudInitParams struct {
	User     string // 缺省 root
	Password string // 必填
	// SSHPort 是 sshd 监听端口（0/22 = 不动缺省）。非缺省时禁 ssh.socket 并按该端口拉起。
	SSHPort int
	// Tunnel443 打开 ssh-over-443：远端装 stunnel4 监听 443 转发到本机 SSHPort，
	// 供出口代理 TUN 劫持直连 22 的本机经 443 隧道接入（P68 家族）。缺省关。
	Tunnel443 bool
}

// CloudInitSpec 在密码 cloud-config 之上，可选注入 sshd 高位端口与 443 隧道。
//
// 这是把「建机置备」从外部编排仓（nmsctl）收回 vpsctl 的落点——原先高位口与
// stunnel 都只活在 nmsctl 的 user-data 模板里。
//
// 三个必须的形态：
//  1. Ubuntu 24.04 的 sshd 由 socket activation 拉起，sshd_config 的 Port 指令被忽略
//     直到禁用 ssh.socket；故先 disable ssh.socket 再经 drop-in 设 Port（P7）。
//  2. 注入体必须 **ASCII-only**（DO user-data 非 ASCII 会被搅成 C1 控制字符，P75）
//     —— 注释一律英文，禁 em dash；由 assertASCII 自守。
//  3. 用 write_files 块标量落文件（内容按行原样，无转义歧义），不用 runcmd 内联
//     printf（YAML 流式标量里 \n 不解释，真机首跑实录）。
func CloudInitSpec(p CloudInitParams) (string, error) {
	user := p.User
	if user == "" {
		user = "root"
	}
	if p.Password == "" {
		return "", errors.New("fleet: 密码为空")
	}
	if strings.ContainsAny(user, "\n\r\x00") {
		return "", errors.New("fleet: ssh_user 含换行/控制字符，无法安全注入 cloud-config")
	}
	if strings.ContainsAny(p.Password, "\n\r\x00") {
		return "", errors.New("fleet: 密码含换行/控制字符，无法安全注入 cloud-config")
	}
	if p.SSHPort != 0 && (p.SSHPort < 1 || p.SSHPort > 65535) {
		return "", fmt.Errorf("fleet: ssh_port %d 越界（1-65535）", p.SSHPort)
	}
	if p.Tunnel443 && p.SSHPort == 0 {
		return "", errors.New("fleet: Tunnel443 需要显式 ssh_port（443 桥到该端口）")
	}
	highPort := p.SSHPort != 0 && p.SSHPort != 22

	var b strings.Builder
	b.WriteString("#cloud-config\n")

	if highPort || p.Tunnel443 {
		b.WriteString("write_files:\n")
	}
	if highPort {
		b.WriteString("  - path: /etc/ssh/sshd_config.d/99-vpsctl.conf\n")
		b.WriteString("    permissions: '0644'\n")
		b.WriteString("    content: |\n")
		fmt.Fprintf(&b, "      Port %d\n", p.SSHPort)
		b.WriteString("      PasswordAuthentication yes\n")
	}
	if p.Tunnel443 {
		// 与 nmsctl user-data 同形（STCONF 段）：443 接收 -> 本机 sshd 端口。
		// cert/key 由下方 runcmd 现签，故此处只写 stunnel 客户端配置。
		b.WriteString("  - path: /etc/stunnel/ssh-443.conf\n")
		b.WriteString("    permissions: '0644'\n")
		b.WriteString("    content: |\n")
		b.WriteString("      cert = /etc/ssl/private/stunnel.pem\n")
		b.WriteString("      key = /etc/ssl/private/stunnel.pem\n")
		b.WriteString("      [ssh-443]\n")
		b.WriteString("      accept = 443\n")
		fmt.Fprintf(&b, "      connect = 127.0.0.1:%d\n", p.SSHPort)
	}

	b.WriteString("ssh_pwauth: true\n")
	b.WriteString("chpasswd:\n")
	b.WriteString("  expire: false\n")
	b.WriteString("  users:\n")
	fmt.Fprintf(&b, "    - name: %s\n", user)
	// YAML 单引号风格：' → ''，其余字符（含反斜杠）原样
	fmt.Fprintf(&b, "      password: '%s'\n", strings.ReplaceAll(p.Password, "'", "''"))
	b.WriteString("      type: text\n")

	if highPort || p.Tunnel443 {
		b.WriteString("runcmd:\n")
		// ASCII only: never use em dash or any non-ASCII byte here (P75).
		if highPort {
			b.WriteString("  - [ systemctl, disable, --now, ssh.socket ]\n")
			b.WriteString("  - [ systemctl, enable, ssh.service ]\n")
			b.WriteString("  - [ systemctl, restart, ssh.service ]\n")
		}
		if p.Tunnel443 {
			// 单引号 YAML 标量外壳：内部可安全用双引号，printf 的 \n 由 shell 解释。
			b.WriteString("  - [ apt-get, install, -y, stunnel4 ]\n")
			b.WriteString("  - [ sh, -c, 'openssl req -new -x509 -days 30 -nodes -subj /CN=vpsctl-nms -keyout /etc/ssl/private/stunnel.key -out /etc/ssl/private/stunnel.crt' ]\n")
			b.WriteString("  - [ sh, -c, 'cat /etc/ssl/private/stunnel.key /etc/ssl/private/stunnel.crt > /etc/ssl/private/stunnel.pem && chmod 600 /etc/ssl/private/stunnel.pem' ]\n")
			b.WriteString("  - [ sh, -c, 'printf \"ENABLED=1\\n\" > /etc/default/stunnel4' ]\n")
			b.WriteString("  - [ systemctl, restart, stunnel4 ]\n")
		}
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
