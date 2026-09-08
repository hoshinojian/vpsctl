package fleet

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestProbeSSHAll(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, _ := ln.Accept()
		if c != nil {
			c.Close()
		}
	}()
	openPort := ln.Addr().(*net.TCPAddr).Port

	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close() // 立刻关闭，连接应被拒

	got := ProbeSSHAll(context.Background(), []string{"127.0.0.1"}, openPort, 2*time.Second)
	if got["127.0.0.1"] != nil {
		t.Errorf("开放端口应可达: %v", got["127.0.0.1"])
	}

	got = ProbeSSHAll(context.Background(), []string{"127.0.0.1"}, closedPort, 2*time.Second)
	if got["127.0.0.1"] == nil {
		t.Error("关闭端口应不可达")
	}

	// 空列表安全
	if got := ProbeSSHAll(context.Background(), nil, openPort, time.Second); len(got) != 0 {
		t.Errorf("空输入应返回空: %v", got)
	}
}
