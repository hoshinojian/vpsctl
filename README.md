# vpsctl

跨 VPS 提供商的节点供给/回收小工具。Go 单二进制、零第三方依赖；多账号配置驱动；
本地 Web 管理台；当前支持 **DigitalOcean**，后续按同一 Provider 接口扩展 Vultr / 阿里云等。

> **凭据安全（本仓库为 public）**：真实 API token 只放本地
> `~/.config/vpsctl/accounts.json`（建议 600 权限），该文件已进 .gitignore，严禁提交。
> 仓内只有占位符示例 `accounts.example.json`。

## 构建

```sh
make build   # 产物 bin/vpsctl
```

## 配置

默认路径 `~/.config/vpsctl/accounts.json`，优先级：`--accounts` 参数 > `VPSCTL_ACCOUNTS` 环境变量 > 默认路径。

```json
{
  "accounts": [
    { "name": "do-1", "provider": "digitalocean", "token": "dop_v1_xxx",
      "ssh_user": "root", "ssh_password": "CHANGE_ME" },
    { "name": "do-2", "provider": "digitalocean", "token": "dop_v1_yyy" }
  ]
}
```

- `name`：账号别名，出现在节点命名与结果 JSON 里，需唯一
- `ssh_user`/`ssh_password`（可选）：账号级 SSH 凭据。配置了密码的账号，
  `create` 时自动注入最小 cloud-config 设密码并开启 SSH 密码登录（与
  `--user-data` 互斥，同给报错）；`list --format nms` 导出 NMS 载荷时带上
- 加载时若文件权限过宽会告警（token 文件建议 `chmod 600`）

## 批量创建：`vpsctl create`

```sh
vpsctl create \
  --region sgp1 --size s-1vcpu-1gb --image ubuntu-24-04-x64 \
  --count 3 --name-prefix vps --ssh-keys <公钥ID|指纹|名称> \
  --tags env:lab --wait 300s --output result.json
```

| 参数 | 说明 |
| --- | --- |
| `--count N` | 每个账号创建台数 |
| `--only team3` | 逗号分隔的账号名，**只在指定账号上创建**（默认全部账号） |
| `--name-prefix` | 命名前缀，最终名 `{prefix}-{account}-{NN}`（如 `vps-do-1-01`） |
| `--start-index N` | 序号起始，跨批次避让重名 |
| `--region/--size/--image` | 必填，slug 可先用 `regions`/`sizes`/`images` 子命令查询 |
| `--ssh-keys` | 逗号分隔，支持 ID / 指纹 / 名称混填（名称自动解析） |
| `--tags` | 附加 tag；自动追加 `batch:<UTC时间戳>` 便于按批筛选 |
| `--user-data` | cloud-init 文件路径 |
| `--wait 300s` | 等待节点 active 且公网 IPv4 就绪（轮询）；0 不等待 |
| `--dry-run` | 只打印创建计划，不调任何 API |
| `--output` | 结果 JSON 另存路径（stdout 始终输出） |

- 多账号并行、账号内并发受限（4），逐台记录成败；**部分失败时 JSON 照常完整输出、退出码 1**
- 输出结构：

```json
{
  "batch": "20260904T153000Z",
  "requested": { "do-1": 3, "do-2": 3 },
  "created": [
    { "account": "do-1", "id": "3164444", "name": "vps-do-1-01", "status": "active",
      "region": "sgp1", "size": "s-1vcpu-1gb", "ipv4_public": "203.0.113.10",
      "price_monthly": 6, "tags": ["batch:20260904T153000Z"], "created_at": "…" }
  ],
  "errors": [ { "account": "do-2", "name": "vps-do-2-02", "index": 2, "error": "…" } ]
}
```

## 节点清单：`vpsctl list`

跨账号拉取全部节点，默认输出 JSON（`--only` 选账号、`--tag`/`--status` 过滤）：

```sh
vpsctl list                          # 全部账号节点清单 JSON
vpsctl list --tag batch:20260908T120000Z --status active
```

### 导出 NMS 台账导入载荷：`--format nms`

输出对齐 nms 仓 API 契约 04 §1.1 的节点对象（`id` 取节点名——重建后同名重导
正好走 NMS 的重录复活通道），可直接喂给 NMS 整网导入（upsert 幂等）：

```sh
vpsctl list --format nms --output nms-nodes.json
curl -s -X POST http://<nms-host>:<port>/api/v1/topology -d @nms-nodes.json
```

- `device_type`/`ssh_port`/`ssh_user` 按缺省显式填好（vps / 22 / 账号级 `ssh_user` 或 root）
- `ssh_password` 取账号级配置，未配置的账号会告警（NMS 录入密码必填，会被拒绝）
- 无公网 IPv4 的节点跳过并提示（NMS 要求合法 `management_ip`）
- 另附 `provider`/`ram_mb`/`disk_gb`/`cpu_cores`/`cost_monthly`/`provisioned_at`
  冗余字段（NMS 现载荷忽略，扩契约后直接可用）
- **载荷含明文密码**：`--output` 文件按 600 权限写入，勿提交进任何仓库

## 管理台：`vpsctl serve`

```sh
vpsctl serve                    # 默认 http://127.0.0.1:8787
vpsctl serve --listen 127.0.0.1:9000
```

- 聚合展示所有账号的节点（区域/状态/标签/名称/IP 过滤，月费合计）
- 勾选后可 **关机 / 开机 / 删除**：
  - **关机**：保留机器、继续计费、随时可再开机（可逆）
  - **删除**：销毁磁盘、停止计费、**不可恢复**。弹窗列出明细与释放月费，需**手动输入台数**确认；服务端再校验请求数量一致
  - 删除可选「先优雅关机再删」：关机未完成（超时 120s / errored）则**不删**——宁可漏删，不可误删
- **创建节点**：工具栏「创建节点…」弹窗里选定账号（以及区域/套餐/镜像/公钥），命名自动续号防重名，月费实时预估；提交后列表自动刷新可见 new → active
- 安全：默认仅监听回环地址、管理台无鉴权（非回环监听会警告）；token 不出后端

## 查询辅助：`vpsctl regions | sizes | images | keys`

按账号分节列出可选 slug / 公钥，用于拼 `create` 参数：

```sh
vpsctl regions --accounts ~/my-accounts.json
vpsctl sizes
vpsctl keys --only team3      # 查询单个账号
```

## 扩展新提供商（Vultr / 阿里云…）

1. 新建 `internal/provider/<name>/`，实现 `provider.Provider` 接口（List/Get/Create/Delete/Power/ActionStatus/SSHKeys/Regions/Sizes/Images）
2. 在 `init()` 里 `provider.Register("<name>", factory)`；`cmd/vpsctl/main.go` 加一条 blank import
3. `accounts.json` 里把对应账号的 `provider` 改为 `<name>` 即可，编排层与 UI 零改动

参考资料（各家官方客户端，端点语义/分页/签名的"标准答案"）：
DigitalOcean [godo](https://github.com/digitalocean/godo)、
Vultr [govultr](https://github.com/vultr/govultr)（游标分页）、
阿里云 [aliyun-cli](https://github.com/aliyun/aliyun-cli)（ACS3-HMAC-SHA256 签名，纯标准库可实现）。

## 开发

```sh
make build / lint / test   # gofmt + go vet；go test -race
make hooks                 # 安装本地 pre-push 钩子：lint+test 全绿才许 push，且禁止直推 main
```

协作流程：feature 分支 → 本地检查全绿 → push → GitHub PR → 人审合并（**无云端 CI**）。
