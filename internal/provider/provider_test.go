package provider_test

import (
	"net/http"
	"testing"

	"github.com/hoshinojian/vpsctl/internal/provider"
	_ "github.com/hoshinojian/vpsctl/internal/provider/digitalocean"
)

func TestRegistry(t *testing.T) {
	p, err := provider.New("digitalocean", "acct", provider.Credentials{Token: "tok"}, http.DefaultClient)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p == nil {
		t.Fatal("返回 nil provider")
	}

	if _, err := provider.New("digitalocean", "acct", provider.Credentials{}, nil); err == nil {
		t.Error("空 token 应报错")
	}
	if _, err := provider.New("nope", "a", provider.Credentials{Token: "t"}, nil); err == nil {
		t.Error("未知提供商应报错")
	}
	names := provider.Names()
	if len(names) == 0 || names[0] != "digitalocean" {
		t.Errorf("Names() 应含 digitalocean 且升序, got %v", names)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Errorf("Names() 未升序: %v", names)
		}
	}
}

// TestAuthKinds：密钥对型与 Token 型互斥，未注册的提供商不在密钥对清单里。
func TestAuthKinds(t *testing.T) {
	keyPairs := provider.KeyPairProviders()
	// 兼容老 cloud-init 的标记按提供商登记；没登记过的（含 DigitalOcean）为 false
	provider.RegisterChpasswdCompat("test-compat")
	if !provider.ChpasswdCompat("test-compat") || provider.ChpasswdCompat("digitalocean") || provider.ChpasswdCompat("nope") {
		t.Errorf("ChpasswdCompat 判定不符: compat=%v do=%v", provider.ChpasswdCompat("test-compat"), provider.ChpasswdCompat("digitalocean"))
	}
	if contains(keyPairs, "digitalocean") {
		t.Errorf("digitalocean 是 Token 型，不应出现在密钥对清单: %v", keyPairs)
	}
	for _, name := range keyPairs {
		if !contains(provider.Names(), name) {
			t.Errorf("密钥对清单里的 %q 未注册", name)
		}
	}
	for i := 1; i < len(keyPairs); i++ {
		if keyPairs[i-1] >= keyPairs[i] {
			t.Errorf("KeyPairProviders() 未升序: %v", keyPairs)
		}
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
