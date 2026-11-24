// Package tencent 是腾讯云 API 3.0 的最小客户端：TC3-HMAC-SHA256 签名、JSON 传输、
// 限流与 5xx 退避重试，只依赖标准库。各产品线（CVM、轻量应用服务器…）的 Provider
// 适配放本包子目录，共用这里的签名与请求入口。
//
// 文档: https://cloud.tencent.com/document/api/213/30654
package tencent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const contentType = "application/json; charset=utf-8"

// signedHeaders 是参与签名的请求头（小写、ASCII 升序）。增减签名头必须同步改
// canonicalHeaders，并与实际发送的头保持一致，否则签名失配。
const signedHeaders = "content-type;host;x-tc-action"

// signTC3 按 TC3-HMAC-SHA256 计算一次 API 3.0 调用的 Authorization 头。
//
// 约定：请求固定 POST + JSON 体（query 为空），签名头固定 content-type / host /
// x-tc-action 三项。service 须与 host 的产品段一致（cvm.tencentcloudapi.com → cvm）。
// CredentialScope 的日期必须由时间戳换算成 UTC+0 日期——用本地时区会在凌晨必然失败。
func signTC3(secretID, secretKey, host, service, action string, payload []byte, now time.Time) string {
	date := now.UTC().Format("2006-01-02")
	scope := date + "/" + service + "/tc3_request"

	// CanonicalRequest 六段：方法、URI（固定 /）、QueryString（POST 为空）、
	// 规范头（每项以 \n 结尾）、签名头列表、请求体哈希。
	canonicalHeaders := "content-type:" + contentType + "\n" +
		"host:" + host + "\n" +
		"x-tc-action:" + strings.ToLower(action) + "\n"
	canonicalRequest := http.MethodPost + "\n/\n\n" +
		canonicalHeaders + "\n" +
		signedHeaders + "\n" +
		hashSHA256(payload)

	stringToSign := "TC3-HMAC-SHA256\n" + strconv.FormatInt(now.Unix(), 10) + "\n" +
		scope + "\n" + hashSHA256([]byte(canonicalRequest))

	// 派生签名密钥三步：日期 → 产品 → 终止串。
	secretDate := hmacSHA256([]byte("TC3"+secretKey), []byte(date))
	secretService := hmacSHA256(secretDate, []byte(service))
	secretSigning := hmacSHA256(secretService, []byte("tc3_request"))
	signature := hex.EncodeToString(hmacSHA256(secretSigning, []byte(stringToSign)))

	return "TC3-HMAC-SHA256 Credential=" + secretID + "/" + scope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + signature
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
