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

	"github.com/hoshinojian/vpsctl/internal/webui"
)

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	accounts := fs.String("accounts", "", "账号配置路径（默认 ~/.config/vpsctl/accounts.json，或 $VPSCTL_ACCOUNTS）")
	listen := fs.String("listen", "127.0.0.1:8787", "监听地址（默认仅本机；管理台无鉴权，勿暴露公网）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadAccounts(*accounts)
	if err != nil {
		return err
	}
	clients, err := buildClients(cfg)
	if err != nil {
		return err
	}

	if host, _, err := net.SplitHostPort(*listen); err == nil {
		if h := host; h != "" && h != "127.0.0.1" && h != "localhost" && h != "::1" {
			fmt.Fprintln(os.Stderr, "警告: 管理台无鉴权且可跨账号删除节点，正在非回环地址上监听，请确认网络可达范围")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	srv := &http.Server{
		Addr:    *listen,
		Handler: webui.New(clients).Handler(),
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
