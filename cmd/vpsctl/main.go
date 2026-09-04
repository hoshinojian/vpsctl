// vpsctl 是跨 VPS 提供商的节点供给/回收小工具（当前支持 DigitalOcean）。
package main

import (
	"fmt"
	"os"

	_ "github.com/hoshinojian/vpsctl/internal/provider/digitalocean"
)

const usage = `vpsctl — VPS 供给/回收工具（DigitalOcean 先行）

用法:
  vpsctl create [flags]             多账号批量创建节点，输出 JSON
  vpsctl serve [flags]              本地 Web 管理台（查看/关机/开机/删除）
  vpsctl regions|sizes|images|keys  查询各账号可选值

子命令帮助: vpsctl <子命令> -h
配置: ~/.config/vpsctl/accounts.json（示例见 accounts.example.json）
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	die := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			os.Exit(1)
		}
	}
	switch os.Args[1] {
	case "create":
		die(runCreate(os.Args[2:]))
	case "serve":
		die(runServe(os.Args[2:]))
	case "regions", "sizes", "images", "keys":
		die(runList(os.Args[1], os.Args[2:]))
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
