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

### 账号管理（CLI 与管理台两端等价）

```sh
vpsctl accounts list                                     # 脱敏列表（密码只显示有无）
vpsctl accounts add --name do-3 --token dop_v1_xxx --ssh-password pw
vpsctl accounts edit --name do-3 --ssh-user deploy --clear-password
vpsctl accounts remove --name do-3 [--force]             # 有节点时要求 --force
```

管理台「账号…」弹窗支持同样操作（新增/编辑/删除），且 serve 运行中**即时生效**；
CLI 改的是文件本身，运行中的 serve 不会自动感知（用管理台改，或改完重启 serve）。

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
| `--name-prefix` | 命名前缀，最终名 `{prefix}-{account}-{region}-{NN}`（如 `vps-do-1-sgp1-01`；region 进名保证跨区域批次不重名，P69） |
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
    { "account": "do-1", "id": "3164444", "name": "vps-do-1-sgp1-01", "status": "active",
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
- **SSH 连通性预检**：导出前对每台探测 22 端口（2s 超时），不通的跳过并提示
  （防止把连不上的机器灌进 NMS 变死台账）；`--no-check-ssh` 可关闭。
  管理台「导出 NMS 载荷」按钮行为一致，跳过数经响应头提示
- 另附 `provider`/`ram_mb`/`disk_gb`/`cpu_cores`/`cost_monthly`/`provisioned_at`
  冗余字段（NMS 现载荷忽略，扩契约后直接可用）
- **载荷含明文密码**：`--output` 文件按 600 权限写入，勿提交进任何仓库

## 生命周期：`vpsctl power | delete`

与 webui 共用同一套实现，防误删语义两端一致。目标二选一：`--tag`（如
`batch:20260908T120000Z`）或 `--ids`（`account/id` 逗号分隔）；`--only` 限定账号范围：

```sh
vpsctl power --tag batch:20260908T120000Z --action off     # 批量关机（可逆，无确认）
vpsctl delete --ids do-1/3164444 --confirm 1
vpsctl delete --tag batch:20260908T120000Z --confirm 3 --shutdown-first
```

- `delete` 必须台数确认：`--confirm N` 与实际目标数一致才执行，不给则交互输入
- `--shutdown-first`：先优雅关机并等待，未完成（超时 120s / errored）则该台**不删**——宁可漏删，不可误删
- 任一账号节点查询失败时整体中止（破坏性操作宁可不动手）；结果逐台 JSON 输出，部分失败退出码 1

## 维护：`vpsctl rebuild | resize`

同样支持 `--tag/--ids/--only` 选目标 + 台数确认，webui 选中节点后的「重装…」「改配…」按钮同语义：

```sh
vpsctl rebuild --ids do-1/3164444 --image ubuntu-24-04-x64 --confirm 1   # 重装（磁盘清空，保留 ID/IP）
vpsctl resize --tag batch:xxx --size s-2vcpu-2gb --confirm 3             # 改配（自动 关机→改配→开机）
vpsctl resize --ids do-1/3164444 --size s-4vcpu-8gb --resize-disk --confirm 1  # 同时扩磁盘（不可逆）
```

- rebuild 数据清空不可恢复；resize 若节点已关机则跳过关机步骤，任一步失败即中止该台
- 改配含两次电源动作，耗时较长（约 2-5 分钟），请耐心等待逐台结果

## 管理台：`vpsctl serve`

```sh
vpsctl serve                    # 默认 http://127.0.0.1:8787
vpsctl serve --listen 127.0.0.1:9000
```

- 聚合展示所有账号的节点（区域/状态/标签/名称/IP 过滤，月费合计）
- **节点详情**：点表格行弹出详情——镜像/磁盘/私网 IP/创建时间/提供商 ID 等完整字段，
  以及该账号的 SSH 用户与密码配置状态（密码永不回传浏览器，只显示已配置/未配置）
- **账号管理**：工具栏「账号…」列出已有账号并可**新增账号**——保存到 accounts.json
  （0600）并立即生效，无需重启 serve
- **导出 NMS 载荷**：工具栏一键下载 04 §1.1 导入载荷（凭据取账号配置），等价于
  `list --format nms`；无公网 IP 的节点自动跳过并提示
- 勾选后可 **关机 / 开机 / 重装 / 改配 / 删除**：
  - **关机**：保留机器、继续计费、随时可再开机（可逆）
  - **重装 / 改配**：与 CLI `rebuild`/`resize` 同语义，弹窗列明细并要求输入台数确认
  - **删除**：销毁磁盘、停止计费、**不可恢复**。弹窗列出明细与释放月费，需**手动输入台数**确认；服务端再校验请求数量一致
  - 删除可选「先优雅关机再删」：关机未完成（超时 120s / errored）则**不删**——宁可漏删，不可误删
- **创建节点**：工具栏「创建节点…」弹窗里选定账号（以及区域/套餐/镜像/公钥），命名自动续号防重名，月费实时预估；提交后列表自动刷新可见 new → active
- 安全：默认仅监听回环地址、管理台无鉴权（非回环监听会警告）；token 不出后端；
  账号管理与 NMS 载荷下载涉及凭据，服务端额外强制回环校验（非 127.0.0.1 一律 403）

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
make check-ui              # 前端冒烟：DOM 桩 + 固定载荷完整执行页面脚本（需 node；pre-push 自动跑）
make hooks                 # 安装本地 pre-push 钩子：lint+test+check-ui 全绿才许 push，且禁止直推 main
```

协作流程：feature 分支 → 本地检查全绿 → push → GitHub PR → 人审合并（**无云端 CI**）。
