package aliyun

import (
	"strings"
	"testing"
)

// TestSignACS3OfficialVector 复现官方文档的完整示例（含最终签名），钉死规范请求串、
// 待签名串与 HMAC 的每一步。示例为 POST + query、空请求体。
// https://help.aliyun.com/zh/sdk/product-overview/v3-request-structure-and-signature
func TestSignACS3OfficialVector(t *testing.T) {
	const (
		host  = "ecs.cn-shanghai.aliyuncs.com"
		query = "ImageId=win2019_1809_x64_dtc_zh-cn_40G_alibase_20230811.vhd&RegionId=cn-shanghai"
	)
	got := signACS3("POST", "YourAccessKeyId", "YourAccessKeySecret", host, query,
		"RunInstances", "2014-05-26", "2023-10-26T10:22:32Z", "3156853299f313e23d1673dc12e1703d")

	// 规范请求串按文档原样重建，哈希应为文档给出的 7ea06492…
	canonical := strings.Join([]string{
		"POST", "/", query,
		"host:" + host + "\n" +
			"x-acs-action:RunInstances\n" +
			"x-acs-content-sha256:" + emptyBodyHash + "\n" +
			"x-acs-date:2023-10-26T10:22:32Z\n" +
			"x-acs-signature-nonce:3156853299f313e23d1673dc12e1703d\n" +
			"x-acs-version:2014-05-26\n",
		signedHeaders,
		emptyBodyHash,
	}, "\n")
	const wantCanonicalHash = "7ea06492da5221eba5297e897ce16e55f964061054b7695beedaac1145b1e259"
	if h := hashSHA256([]byte(canonical)); h != wantCanonicalHash {
		t.Errorf("规范请求哈希 = %s, want %s\n规范请求串:\n%s", h, wantCanonicalHash, canonical)
	}
	const want = "ACS3-HMAC-SHA256 Credential=YourAccessKeyId," + "SignedHeaders=" + signedHeaders +
		",Signature=06563a9e1b43f5dfe96b81484da74bceab24a1d853912eee15083a6f0f3283c0"
	if got != want {
		t.Errorf("Authorization 不符:\n got %s\nwant %s", got, want)
	}
}

// TestSignACS3InputsMatter：方法、密钥、域名、query、接口名、版本、时间、nonce
// 任一变化都应改变签名。
func TestSignACS3InputsMatter(t *testing.T) {
	sign := func(method, id, secret, host, query, action, version, date, nonce string) string {
		return signACS3(method, id, secret, host, query, action, version, date, nonce)
	}
	base := sign("GET", "id", "secret", "ecs.aliyuncs.com", "RegionId=cn-hangzhou", "DescribeRegions", "2014-05-26", "2026-11-24T12:00:00Z", "nonce")
	variants := map[string]string{
		"method":  sign("POST", "id", "secret", "ecs.aliyuncs.com", "RegionId=cn-hangzhou", "DescribeRegions", "2014-05-26", "2026-11-24T12:00:00Z", "nonce"),
		"id":      sign("GET", "id2", "secret", "ecs.aliyuncs.com", "RegionId=cn-hangzhou", "DescribeRegions", "2014-05-26", "2026-11-24T12:00:00Z", "nonce"),
		"secret":  sign("GET", "id", "secret2", "ecs.aliyuncs.com", "RegionId=cn-hangzhou", "DescribeRegions", "2014-05-26", "2026-11-24T12:00:00Z", "nonce"),
		"host":    sign("GET", "id", "secret", "ecs.cn-hangzhou.aliyuncs.com", "RegionId=cn-hangzhou", "DescribeRegions", "2014-05-26", "2026-11-24T12:00:00Z", "nonce"),
		"query":   sign("GET", "id", "secret", "ecs.aliyuncs.com", "RegionId=cn-beijing", "DescribeRegions", "2014-05-26", "2026-11-24T12:00:00Z", "nonce"),
		"action":  sign("GET", "id", "secret", "ecs.aliyuncs.com", "RegionId=cn-hangzhou", "DescribeZones", "2014-05-26", "2026-11-24T12:00:00Z", "nonce"),
		"version": sign("GET", "id", "secret", "ecs.aliyuncs.com", "RegionId=cn-hangzhou", "DescribeRegions", "2014-05-27", "2026-11-24T12:00:00Z", "nonce"),
		"date":    sign("GET", "id", "secret", "ecs.aliyuncs.com", "RegionId=cn-hangzhou", "DescribeRegions", "2014-05-26", "2026-11-24T12:00:01Z", "nonce"),
		"nonce":   sign("GET", "id", "secret", "ecs.aliyuncs.com", "RegionId=cn-hangzhou", "DescribeRegions", "2014-05-26", "2026-11-24T12:00:00Z", "nonce2"),
	}
	for name, sig := range variants {
		if sig == base {
			t.Errorf("改变 %s 后签名不应不变", name)
		}
	}
}

func TestPercentEncode(t *testing.T) {
	cases := map[string]string{
		"abcXYZ019-_.~": "abcXYZ019-_.~", // 非保留字符原样
		"a b":           "a%20b",         // 空格是 %20，不是 +
		"a/b":           "a%2Fb",
		"a*b":           "a%2Ab", // 星号要编码（不是 Python quote 的安全字符）
		"a+b":           "a%2Bb",
		"a=b&c":         "a%3Db%26c",
		"中文":            "%E4%B8%AD%E6%96%87", // UTF-8 逐字节
		"":              "",
	}
	for in, want := range cases {
		if got := percentEncode(in); got != want {
			t.Errorf("percentEncode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalQuery(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]string
		want   string
	}{
		{"按参数名升序", map[string]string{"RegionId": "cn-hangzhou", "ImageId": "ubuntu_24_04"}, "ImageId=ubuntu_24_04&RegionId=cn-hangzhou"},
		{"空值丢弃", map[string]string{"RegionId": "cn-hangzhou", "ZoneId": "", "InstanceName": ""}, "RegionId=cn-hangzhou"},
		{"数组参数按下标平铺后仍按名排序", map[string]string{
			"Tag.2.Value": "batch:2", "Tag.1.Key": "batch", "Tag.2.Key": "owner", "Tag.1.Value": "batch:1",
		}, "Tag.1.Key=batch&Tag.1.Value=batch%3A1&Tag.2.Key=owner&Tag.2.Value=batch%3A2"},
		{"全空", map[string]string{"A": ""}, ""},
		{"nil", nil, ""},
	}
	for _, c := range cases {
		if got := canonicalQuery(c.params); got != c.want {
			t.Errorf("%s: canonicalQuery = %q, want %q", c.name, got, c.want)
		}
	}
}
