package digitalocean

import (
	"net/http"
	"testing"
)

func TestNewDefaultClientHasTimeout(t *testing.T) {
	c := New("a", "tok", nil)
	if c.http == nil || c.http.Timeout <= 0 {
		t.Errorf("默认客户端必须带超时（防上游挂起拖死调用方）, got %+v", c.http)
	}
	custom := &http.Client{}
	c2 := New("a", "tok", custom)
	if c2.http != custom {
		t.Error("自定义 client 应原样使用")
	}
}
