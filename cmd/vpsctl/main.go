// vpsctl 是跨 VPS 提供商的节点供给/回收小工具（当前支持 DigitalOcean）。
package main

import (
	"fmt"
	"os"
)

const usage = `vpsctl — VPS 供给/回收工具（DigitalOcean 先行）

用法:
  vpsctl create                     批量创建节点，输出 JSON
  vpsctl serve                      本地 Web 管理台（查看/关机/开机/删除）
  vpsctl regions|sizes|images|keys  查询各账号可选值

配置: ~/.config/vpsctl/accounts.json（示例见 accounts.example.json）
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "create", "serve", "regions", "sizes", "images", "keys":
		fmt.Fprintf(os.Stderr, "vpsctl %s: 尚未实现（脚手架阶段）\n", os.Args[1])
		os.Exit(2)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
