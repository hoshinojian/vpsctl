// Package aliyun 是阿里云 OpenAPI（RPC 风格）的最小客户端：ACS3-HMAC-SHA256 签名、
// GET + query 传输、限流与 5xx 退避重试，只依赖标准库。各产品线（ECS、轻量应用
// 服务器…）的 Provider 适配放本包子目录，共用这里的签名与请求入口。
//
// 文档: https://help.aliyun.com/zh/sdk/product-overview/v3-request-structure-and-signature
package aliyun

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
)

// emptyBodyHash 是空请求体的 SHA-256；请求体恒为空（业务参数全走 query）。
const emptyBodyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// signedHeaders 是参与签名的请求头（小写、ASCII 升序）。除 Authorization 外，
// host、content-type 与全部 x-acs-* 头都必须签名——实际发送的头多一个、少一个
// 或值不同都会 SignatureNotMatch。
const signedHeaders = "host;x-acs-action;x-acs-content-sha256;x-acs-date;x-acs-signature-nonce;x-acs-version"

// signACS3 按 ACS3-HMAC-SHA256 计算一次 RPC 调用的 Authorization 头。
// Action 与 Version 走头部，业务参数全部编进 canonicalQuery；date 为 ISO 8601 UTC
// （2006-01-02T15:04:05Z），nonce 每次请求唯一。
func signACS3(method, accessKeyID, accessKeySecret, host, canonicalQuery, action, version, date, nonce string) string {
	canonicalHeaders := "host:" + host + "\n" +
		"x-acs-action:" + action + "\n" +
		"x-acs-content-sha256:" + emptyBodyHash + "\n" +
		"x-acs-date:" + date + "\n" +
		"x-acs-signature-nonce:" + nonce + "\n" +
		"x-acs-version:" + version + "\n"
	canonicalRequest := method + "\n/\n" + canonicalQuery + "\n" +
		canonicalHeaders + "\n" + signedHeaders + "\n" + emptyBodyHash
	stringToSign := "ACS3-HMAC-SHA256\n" + hashSHA256([]byte(canonicalRequest))
	signature := hex.EncodeToString(hmacSHA256([]byte(accessKeySecret), []byte(stringToSign)))
	return "ACS3-HMAC-SHA256 Credential=" + accessKeyID +
		",SignedHeaders=" + signedHeaders + ",Signature=" + signature
}

// canonicalQuery 把业务参数编成规范查询串：参数名升序，名与值分别按 RFC 3986
// 百分号编码，空值参数丢弃。返回值同时用于签名与实际请求 URL，保证两边一致。
func canonicalQuery(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if v != "" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = percentEncode(k) + "=" + percentEncode(params[k])
	}
	return strings.Join(parts, "&")
}

// percentEncode 按 RFC 3986 百分号编码：A-Z a-z 0-9 - _ . ~ 不编码，其余字节转
// 大写 %XX（空格是 %20 而非 +，斜杠是 %2F）。
func percentEncode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			const hexDigits = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0F])
		}
	}
	return b.String()
}

func hmacSHA256(key, msg []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return h.Sum(nil)
}

func hashSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
