// password.go：查看 vpsctl 建机时给节点生成的随机 root 密码。只读本机密码库
// （账号配置旁边的 node-passwords.json），不调任何 API；密码会明文打到 stdout，
// 只在确实要用的时候查。
package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/hoshinojian/vpsctl/internal/nodepass"
)

func runPassword(args []string) error {
	fs := flag.NewFlagSet("password", flag.ExitOnError)
	accounts := fs.String("accounts", "", "账号配置路径（默认 ~/.config/vpsctl/accounts.json，或 $VPSCTL_ACCOUNTS）")
	ids := fs.String("ids", "", "逗号分隔的 账号/实例ID（必填，可用 vpsctl list 查询）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	keys := splitCSV(*ids)
	if len(keys) == 0 {
		return errors.New("--ids 必填（逗号分隔的 账号/实例ID）")
	}
	_, cfgPath, err := loadAccounts(*accounts)
	if err != nil {
		return err
	}
	stored, err := nodepass.Open(nodepass.PathFor(cfgPath)).Load()
	if err != nil {
		return err
	}
	out := make(map[string]nodepass.Entry, len(keys))
	var missing []string
	for _, k := range keys {
		e, ok := stored[k]
		if !ok {
			missing = append(missing, k)
			continue
		}
		out[k] = e
	}
	if len(missing) > 0 {
		return fmt.Errorf("密码库里没有这些节点的记录（只记录 vpsctl 自己建机时生成的随机密码；存量节点用的是账号级 ssh_password）: %s", strings.Join(missing, ", "))
	}
	return printJSON(out, "")
}
