package provider_test

import (
	"net/http"
	"testing"

	"github.com/hoshinojian/vpsctl/internal/provider"
	_ "github.com/hoshinojian/vpsctl/internal/provider/digitalocean"
)

func TestRegistry(t *testing.T) {
	p, err := provider.New("digitalocean", "acct", "tok", http.DefaultClient)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p == nil {
		t.Fatal("返回 nil provider")
	}

	if _, err := provider.New("digitalocean", "acct", "", nil); err == nil {
		t.Error("空 token 应报错")
	}
	if _, err := provider.New("nope", "a", "t", nil); err == nil {
		t.Error("未知提供商应报错")
	}
	if names := provider.Names(); len(names) != 1 || names[0] != "digitalocean" {
		t.Errorf("Names() = %v", names)
	}
}
