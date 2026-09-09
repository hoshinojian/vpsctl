package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/hoshinojian/vpsctl/internal/webui"
)

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	accounts := fs.String("accounts", "", "账号配置路径（默认 ~/.config/vpsctl/accounts.json，或 $VPSCTL_ACCOUNTS）")
	listen := fs.String("listen", "127.0.0.1:8787", "监听地址（默认仅本机；管理台无鉴权，勿暴露公网）")
	ui := fs.String("ui", "", "前端开发目录：存在时从该目录读 index.html（免重编译热改；不设则用内嵌）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, cfgPath, err := loadAccounts(*accounts)
	if err != nil {
		return err
	}
	clients, err := buildClients(cfg)
	if err != nil {
		return err
	}

	if host, _, err := net.SplitHostPort(*listen); err == nil {
		if h := host; h != "" && h != "127.0.0.1" && h != "localhost" && h != "::1" {
			fmt.Fprintln(os.Stderr, "警告: 管理台无鉴权，且可跨账号关机/删除/重装节点。")
			fmt.Fprintf(os.Stderr, "      当前监听在非回环地址 %s，任何能访问该端口的机器都可直接操作你的 VPS。\n", *listen)
			fmt.Fprintln(os.Stderr, "      建议仅 127.0.0.1 监听，或在前加一层带鉴权的反向代理。")
		}
	}

	web := webui.New(clients, cfg, cfgPath)
	if *ui != "" {
		web.SetUIDir(*ui)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	srv := &http.Server{
		Addr:              *listen,
		Handler:           web.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	fmt.Printf("vpsctl 管理台: http://%s （Ctrl-C 退出）\n", *listen)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		fmt.Println("\n退出中…")
		return srv.Close()
	}
}
