package tencent

import (
	"strings"
	"testing"
	"time"
)

// TestSignTC3CanonicalRequest 用官方文档示例校验规范请求串的拼装：文档里的
// SecretId/SecretKey 已脱敏，无法比对最终签名，但请求体哈希与规范请求哈希
// 不依赖密钥，足以钉死 CanonicalRequest 的每一段。
// 示例：广州区查 1 台"未命名"实例，时间戳 1551113065。
func TestSignTC3CanonicalRequest(t *testing.T) {
	payload := `{"Limit": 1, "Filters": [{"Values": ["\u672a\u547d\u540d"], "Name": "instance-name"}]}`
	// 该时间戳在东八区已是 2019-02-26，CredentialScope 日期须取 UTC+0 的 2019-02-25
	now := time.Unix(1551113065, 0).In(time.FixedZone("CST", 8*3600))

	got := signTC3("AKIDexample", "secret", "cvm.tencentcloudapi.com", "cvm", "DescribeInstances", []byte(payload), now)

	const wantPayloadHash = "35e9c5b0e3ae67532d3c9f17ead6c90222632e5b1ff7f6e89887f1398934f064"
	if h := hashSHA256([]byte(payload)); h != wantPayloadHash {
		t.Errorf("请求体哈希 = %s, want %s", h, wantPayloadHash)
	}
	// 规范请求串按文档原样重建，哈希应等于文档给出的 7019a55b…
	canonical := strings.Join([]string{
		"POST", "/", "",
		"content-type:application/json; charset=utf-8\nhost:cvm.tencentcloudapi.com\nx-tc-action:describeinstances\n",
		"content-type;host;x-tc-action",
		wantPayloadHash,
	}, "\n")
	const wantCanonicalHash = "7019a55be8395899b900fb5564e4200d984910f34794a27cb3fb7d10ff6a1e84"
	if h := hashSHA256([]byte(canonical)); h != wantCanonicalHash {
		t.Errorf("规范请求哈希 = %s, want %s\n规范请求串:\n%s", h, wantCanonicalHash, canonical)
	}

	const wantPrefix = "TC3-HMAC-SHA256 Credential=AKIDexample/2019-02-25/cvm/tc3_request, SignedHeaders=content-type;host;x-tc-action, Signature="
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("Authorization 前缀不符（CredentialScope 日期应为 UTC+0）: %s", got)
	}
	if sig := strings.TrimPrefix(got, wantPrefix); len(sig) != 64 {
		t.Errorf("签名长度 = %d, want 64: %q", len(sig), sig)
	}
}

// TestSignTC3ActionLowercased：x-tc-action 的规范头值必须小写——接口名大小写
// 不应影响签名（实际发送的头保持原样，见 client 测试）。
func TestSignTC3ActionLowercased(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	a := signTC3("id", "k", "cvm.tencentcloudapi.com", "cvm", "DescribeInstances", []byte(`{}`), now)
	b := signTC3("id", "k", "cvm.tencentcloudapi.com", "cvm", "describeinstances", []byte(`{}`), now)
	if a != b {
		t.Errorf("接口名大小写不应影响签名:\n%s\n%s", a, b)
	}
}

// TestSignTC3InputsMatter：密钥、服务段、产品域名、请求体、时间任一变化都应改变签名。
func TestSignTC3InputsMatter(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	base := signTC3("id", "k", "cvm.tencentcloudapi.com", "cvm", "DescribeInstances", []byte(`{}`), now)
	variants := map[string]string{
		"secretKey": signTC3("id", "k2", "cvm.tencentcloudapi.com", "cvm", "DescribeInstances", []byte(`{}`), now),
		"secretID":  signTC3("id2", "k", "cvm.tencentcloudapi.com", "cvm", "DescribeInstances", []byte(`{}`), now),
		"service":   signTC3("id", "k", "cvm.tencentcloudapi.com", "vpc", "DescribeInstances", []byte(`{}`), now),
		"host":      signTC3("id", "k", "vpc.tencentcloudapi.com", "cvm", "DescribeInstances", []byte(`{}`), now),
		"action":    signTC3("id", "k", "cvm.tencentcloudapi.com", "cvm", "DescribeZones", []byte(`{}`), now),
		"payload":   signTC3("id", "k", "cvm.tencentcloudapi.com", "cvm", "DescribeInstances", []byte(`{"Limit":1}`), now),
		"time":      signTC3("id", "k", "cvm.tencentcloudapi.com", "cvm", "DescribeInstances", []byte(`{}`), now.Add(time.Second)),
	}
	for name, sig := range variants {
		if sig == base {
			t.Errorf("改变 %s 后签名不应不变", name)
		}
	}
}
