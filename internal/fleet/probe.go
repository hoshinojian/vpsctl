package fleet

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"
)

// ProbeSSHAll 并发探测每个主机的指定端口（SSH 导出预检用，port 通常 22）；
// 返回 host → 错误（nil = 可达）。同一 host 只探测一次，单台超时 timeout。
func ProbeSSHAll(ctx context.Context, hosts []string, port int, timeout time.Duration) map[string]error {
	uniq := make([]string, 0, len(hosts))
	seen := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		if h != "" && !seen[h] {
			seen[h] = true
			uniq = append(uniq, h)
		}
	}
	out := make(map[string]error, len(uniq))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, h := range uniq {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			d := net.Dialer{Timeout: timeout}
			conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(h, strconv.Itoa(port)))
			if conn != nil {
				conn.Close()
			}
			mu.Lock()
			defer mu.Unlock()
			out[h] = err
		}(h)
	}
	wg.Wait()
	return out
}
