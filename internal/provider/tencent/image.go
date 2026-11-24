package tencent

import (
	"crypto/md5" //nolint:gosec // 公钥指纹算法本身是 MD5，不是安全用途
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

// imageVersion 抓镜像名里的第一个"主版本[.次版本]"，如 "Ubuntu Server 24.04 LTS 64bit"
// → 24.04。镜像对象的 OsVersion 字段不可靠（新镜像常是 0.0.0），只能从名字里取。
var imageVersion = regexp.MustCompile(`(\d+)(?:\.(\d+))?`)

// ImageSlug 由镜像的发行版与版本号生成跨地域稳定的 slug：小写发行版名 + 主版本
// [+ 两位次版本]，如 ubuntu-24.04、centos-7.09、opencloudos-9.02。云服务器与轻量
// 应用服务器的同一套公共镜像得到同一个 slug，换产品线不用换 --image。
func ImageSlug(platform, name string) string {
	distro := strings.ToLower(strings.Join(strings.Fields(platform), "-"))
	m := imageVersion.FindStringSubmatch(name)
	if m == nil {
		return distro
	}
	major, minor := m[1], m[2]
	if len(minor) == 1 {
		minor = "0" + minor
	}
	if minor == "" || minor == "00" {
		return distro + "-" + major
	}
	return distro + "-" + major + "." + minor
}

// SSHFingerprint 按 OpenSSH 的算法算公钥指纹：base64 解码后的 MD5，小写冒号分隔
// 十六进制（与 DigitalOcean 的指纹写法一致）。公钥格式不对时返回空串。
func SSHFingerprint(pub string) string {
	fields := strings.Fields(pub)
	if len(fields) < 2 {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return ""
	}
	sum := md5.Sum(raw) //nolint:gosec
	parts := make([]string, 0, len(sum))
	for _, b := range sum {
		parts = append(parts, fmt.Sprintf("%02x", b))
	}
	return strings.Join(parts, ":")
}
