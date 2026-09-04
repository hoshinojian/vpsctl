# vpsctl

跨 VPS 提供商的节点供给/回收小工具（当前支持 DigitalOcean）。
Go 单二进制、零第三方依赖；多账号配置驱动。

> 凭据安全：真实 API token 只放本地 `~/.config/vpsctl/accounts.json`（建议 600 权限），
> 该文件已进 .gitignore，严禁提交。仓内只有占位符示例 `accounts.example.json`。

## 子命令

| 子命令 | 用途 | 状态 |
| --- | --- | --- |
| `vpsctl create` | 多账号批量创建节点，输出 JSON | 开发中 |
| `vpsctl serve` | 本地 Web 管理台（查看/关机/开机/删除） | 开发中 |
| `vpsctl regions` `sizes` `images` `keys` | 查询各账号可选值 | 开发中 |

## 配置

默认路径 `~/.config/vpsctl/accounts.json`，可用 `--accounts` 参数或 `VPSCTL_ACCOUNTS` 环境变量覆盖：

```json
{
  "accounts": [
    { "name": "do-1", "provider": "digitalocean", "token": "dop_v1_xxx" }
  ]
}
```

## 开发

```sh
make build   # 编译到 bin/vpsctl
make lint    # gofmt + go vet
make test    # go test -race ./...
make hooks   # 安装本地 pre-push 钩子：lint+test 全绿才许 push，且禁止直推 main
```

协作流程：feature 分支开发 → 本地检查全绿 → push → GitHub PR → 人审合并（无云端 CI）。
