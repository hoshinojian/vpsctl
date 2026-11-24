# vpsctl

跨 VPS 提供商的节点供给/回收小工具。Go 单二进制、零第三方依赖；多账号配置驱动；
本地 Web 管理台；当前支持 **DigitalOcean、腾讯云 CVM、阿里云 ECS**（三家都按量计费、随建随删）与
**腾讯云轻量应用服务器**（包年包月），后续按同一 Provider 接口继续扩展。

> **凭据安全（本仓库为 public）**：真实 API token 只放本地
> `~/.config/vpsctl/accounts.json`（建议 600 权限）和节点密码库
> `~/.config/vpsctl/node-passwords.json`，两者都已进 .gitignore，严禁提交。
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
    { "name": "do-2", "provider": "digitalocean", "token": "dop_v1_yyy" },
    { "name": "tc-1", "provider": "tencent-cvm",
      "access_key": "AKID_xxx", "secret_key": "腾讯云 SecretKey" },
    { "name": "ali-1", "provider": "aliyun-ecs",
      "access_key": "LTAI_xxx", "secret_key": "阿里云 AccessKey Secret" }
  ]
}
```

- `name`：账号别名，出现在节点命名与结果 JSON 里，需唯一
- 凭据二选一：`provider: digitalocean` 填 `token`；`provider: tencent-cvm` / `aliyun-ecs` 填
  `access_key` + `secret_key`（腾讯云是 SecretId / SecretKey，阿里云是 AccessKey ID / Secret）
- `ssh_user`（可选）：建机注入密码时设给哪个用户，缺省 `root`；导出 NMS 载荷时也带上
- `ssh_password`（可选，只给存量节点兜底）：vpsctl 建机时给**每台节点生成一个随机 root 密码**
  （见「每台节点一个随机 root 密码」），不再用账号级密码；`ssh_password` 只在导出 NMS 载荷时
  给"密码库里没有记录的节点"（老版本建的、别的工具建的）兜底
- 加载时若文件权限过宽会告警（token 文件建议 `chmod 600`）

### 账号管理（CLI 与管理台两端等价）

```sh
vpsctl accounts list                                     # 脱敏列表（密码只显示有无）
vpsctl accounts add --name do-3 --token dop_v1_xxx --ssh-password pw
vpsctl accounts add --name tc-1 --provider tencent-cvm --access-key AKID_xxx --secret-key xxx
vpsctl accounts edit --name tc-1 --access-key AKID_yyy --secret-key yyy   # 密钥对必须成对改
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
| `--user-data` | cloud-init 文件路径（原样下发，不注入随机密码） |
| `--no-password` | 什么都不注入（缺省每台节点一个随机 root 密码并记入本地密码库） |
| `--wait 300s` | 等待节点 active 且公网 IPv4 就绪（轮询）；0 不等待 |
| `--dry-run` | 只打印创建计划，不调任何 API |
| `--check` | 只做建机前的提供商预检（云厂商的 DryRun），不创建任何资源 |
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

### 每台节点一个随机 root 密码

`vpsctl create` 与管理台「创建节点」缺省给**每台节点生成一个互不相同的随机 root 密码**，经
cloud-init 注入，并记入账号配置旁边的本地密码库 `~/.config/vpsctl/node-passwords.json`
（0600，按"账号/实例ID"记录）：

- 密码只进本地密码库：不进 create 的结果 JSON，也不回传管理台
- `list --format nms` 与管理台「导出 NMS 载荷」逐台带各自的密码；密码库里没有记录的节点
  （存量节点、别的工具建的）回落到账号级 `ssh_password`
- 要看某台节点的密码：`vpsctl password --ids do-1/3164444`（明文打到 stdout，只在要用时查）
- 节点销毁时记录一并清掉；重装、改配、关开机后密码不变
- 不想要随机密码：`--no-password` 什么都不注入——这时裸机保护照旧（没有 `--user-data` /
  `--ssh-keys` 的机器无法 SSH 管理，要加 `--allow-bare` 放行）；`--user-data` 则原样下发你给的
  cloud-config。这两种方式下，配了 `ssh_password` 的账号会被拦下：导出会带一个机器上没有的密码
- 建机前先确认密码库写得进去，写不进去就不建机；建完才发现写不进去的极端情况会把密码打到
  stderr，请立即保存

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
- **创建节点**：工具栏「创建节点…」弹窗里选定账号（以及区域/套餐/镜像/公钥），命名自动续号防重名，月费实时预估；提交后列表自动刷新可见 new → active。每台节点一个随机 root 密码并记入本地密码库（与 CLI 缺省一致）
- 安全：默认仅监听回环地址、管理台无鉴权（非回环监听会警告）；token 不出后端；
  账号管理与 NMS 载荷下载涉及凭据，服务端额外强制回环校验（非 127.0.0.1 一律 403）

## 查询辅助：`vpsctl regions | sizes | images | keys`

按账号分节列出可选 slug / 公钥，用于拼 `create` 参数：

```sh
vpsctl regions --accounts ~/my-accounts.json
vpsctl sizes
vpsctl keys --only team3      # 查询单个账号
```

- `sizes` 的月费列带币种（`6.00 USD` / `146.00 CNY`）；没有目录价的机型打 `—`

## 腾讯云 CVM / 阿里云 ECS

两家都走**云服务器**产品线、**按量计费**（和 DO 一样按小时付费、随建随删）。编排层与管理台
无感，差异全部吸收在 `internal/provider/tencent/cvm/` 与 `internal/provider/aliyun/ecs/`：

| | 腾讯云 `tencent-cvm` | 阿里云 `aliyun-ecs` |
| --- | --- | --- |
| 凭据 | 子用户 SecretId / SecretKey | RAM 用户 AccessKey ID / Secret |
| 区域 slug | 地域 `ap-beijing`，或可用区 `ap-beijing-8` | 地域 `cn-qingdao`，或可用区 `cn-qingdao-b` |
| 套餐 slug | 机型名，如 `SA5.MEDIUM2` | 规格 ID，如 `ecs.u1-c1m1.large` |
| 镜像 slug | `ubuntu-24.04`、`centos-7.09`…（跨地域稳定），也可直接给 ImageId | 同左 |
| 月费 | 按量小时价 × 730 折算，`currency` 为 `CNY` | 同左 |

```sh
vpsctl sizes  --only tc-1     # 看可选机型
vpsctl images --only tc-1     # 看可选镜像
vpsctl create --only tc-1 --region ap-beijing --size SA5.MEDIUM2 --image ubuntu-24.04 \
  --count 1 --name-prefix vps --wait 300s
```

- **网络**：落在该地域的默认 VPC / 默认交换机上（区域给了可用区就取该可用区的子网），不代建
  网络——地域没有默认 VPC（腾讯云）或没有交换机（阿里云）时报错，请先在控制台建好
- **安全组**：腾讯云不指定，由平台绑默认安全组（没有则自动创建一个）；阿里云取交换机所在
  VPC 的安全组
- **系统盘类型**随机型而异、没有稳妥的查询口：建机前用 `DryRun` 逐个试（腾讯云 增强型 SSD →
  高性能云盘 → …；阿里云 ESSD → ESSD Entry → 高效云盘 → …），第一个通过的才真建。`DryRun`
  同时是整体预检，参数 / 库存问题在扣费前就报出来
- **随机 root 密码**照旧经 cloud-init 注入。腾讯云的公共镜像带的是定制版
  cloud-init 20.1，不认 22.3 才有的 `chpasswd.users` 写法（密码会静默不生效），所以两家建机
  都同时下发老的 `chpasswd.list` 写法（新版本两种都处理，阿里云 23.2.2 实测）。两家 Ubuntu 镜像实测 root 密码都可直接 SSH，重装后密码仍有效
- **关开机 / 重装 / 改配后公网 IP 不变**，纳管用的地址不会漂
- **改配不含磁盘扩容**：只调机型 / 规格，`resize --resize-disk` 直接报错（扩盘请去控制台）。
  改配完成后平台还要收尾一小会儿，这期间的开机请求 vpsctl 会自动等待重试
- **重装**是 关机 → 重装系统盘 → 开机，保留实例 ID / 公网 IP。镜像是否适配机型只能由平台判定：
  建机时 `DryRun` 预检会拦下；重装时才被拒的话系统盘不会动，实例会恢复开机
- **建机前可先 `--check`**：只做提供商预检（云厂商的 DryRun），不创建任何资源
- **一次未复现的故障留档**：腾讯云有一台测试机在"重装完成约 30 秒后改配"之后 22 端口被拒
  （实例显示运行中、能 ping 通），关机再开机无效，重装后恢复；之后 4 次相同或相近的操作都正常，
  原因未查明。遇到类似情况先用 `vpsctl rebuild` 恢复
- **`sizes` 目录**只列每系列最新一代、≤8 vCPU 的通用机型，且**不带价格**（两家都没有批量询价）；
  月费在 `list` 里逐台询价。别的机型直接把机型名写进 `--size`
- **列表较慢**：两家都是地域型 API，`list` 要逐地域并发查（阿里云 33 个地域，受最慢的海外
  接入点拖累，一次约 5–10 秒）；管理台"月费合计"按币种分别相加，不同币种不混算
- **账号下的存量机器**也会一并列出、可被选中操作——给 vpsctl 配密钥前先想清楚这一点

权限与验证（2026-11-24 实测，用的是各自账号里的子用户 / RAM 用户）：

- 两家各用一台最小规格的按量实例做过全链路真机验证：建机（注入 root 密码）→ 密码 SSH →
  关开机 → 重装 → 改配 → 销毁（云盘无残留）
- 列表、开关机、重装、改配、销毁只需要云服务器相关的读写权限（腾讯云建机还要能读默认
  VPC / 子网）
- 腾讯云**建机另需付款权限**——主账号在「访问管理 → 用户列表 → vpsctl → 关联策略」加
  `QCloudFinanceFullAccess`，否则报
  `UnauthorizedOperation: Since you have no payment rights and cannot complete the payment`。
  这一条 `DryRun` 预检发现不了（预检会通过，真下单才被拒）

## 腾讯云轻量应用服务器（`tencent-lighthouse`）

**包年包月**：建一次就付一个月，到期自动续费。和 DO 那种"按小时、随建随删"是两种用法——
适合长期在线的节点，不适合做实验。阿里云轻量应用服务器暂未支持（它的 OpenAPI 没有公开的
销毁接口，也没有关闭自动续费的接口，做不了"跟 DO 一样"）。

```sh
vpsctl sizes  --only lh-1                        # 套餐自带月价（广州 2 核 2G 约 ¥35–55/月）
vpsctl images --only lh-1                        # 镜像 slug 与云服务器同一套（ubuntu-24.04…）
vpsctl create --only lh-1 --region ap-guangzhou --size bundle_starter_mc_med2_03 \
  --image ubuntu-24.04 --count 1 --check         # 先只预检，不花钱
vpsctl create --only lh-1 --region ap-guangzhou --size bundle_starter_mc_med2_03 \
  --image ubuntu-24.04 --count 1 --wait 300s     # 真建：付一个月，自动续费
```

- **`--size` 是套餐 ID**（`vpsctl sizes` 可查），不是机型名；区域可以是地域或可用区
- **每次建机先发一遍预检**（平台的 DryRun），参数 / 库存 / 余额的问题在扣费前报出来；
  `--check` 只走到这一步
- **只能升配、不能降配**：`resize` 换更贵的套餐；`--resize-disk` 报错（要更大的盘选磁盘更大的套餐）
- **停机**：`vpsctl delete`（销毁，退款按平台的退还规则走）。不想续费了也可以先在控制台
  关掉自动续费
- **不支持靠 cloud-init 的置备**：自定义 `--user-data`、高位口（`--ssh-port`）、443 隧道、
  `--authorized-keys` 都会在建机前被明确拒绝；登录密钥用 `--ssh-keys`
- **随机 root 密码走平台的"登录密码"**：平台的密码落在镜像的默认用户上（Ubuntu 是
  `ubuntu`），root 默认禁止 SSH 登录。所以建机时顺带下发一条平台自动执行的命令，等默认
  用户的密码落盘后把**密码哈希**复制给 root 并打开 root 密码登录——命令会留在平台的命令
  记录里，所以里面不出现明文密码

> **这一节的真机验证还没做。** 建机预检、地域 / 套餐 / 镜像 / 密钥对查询、全部读写接口的
> 鉴权都在真实账号上过了；但建机之后的流程（平台自动执行的命令是否真能把 root 打开、
> 开关机、重装、升配、销毁）只有单元测试，没有在真机上跑过——建一台就要付一个月。

## 扩展新提供商

1. 新建 `internal/provider/<name>/`，实现 `provider.Provider` 接口（List/Get/Create/Delete/Power/ActionStatus/Rebuild/Resize/SSHKeys/Regions/Sizes/Images）
2. 在 `init()` 里 `provider.Register("<name>", provider.AuthToken 或 provider.AuthKeyPair, factory)`
   ——第二个参数决定配置与管理台让用户填 Token 还是 AccessKey + Secret；`cmd/vpsctl/main.go`
   加一条 blank import
3. `accounts.json` 里把对应账号的 `provider` 改为 `<name>` 即可，编排层与 UI 零改动

现成可复用的部件：

- `internal/provider/tencent`（TC3-HMAC-SHA256 签名）、`internal/provider/aliyun`（ACS3-HMAC-SHA256
  签名）：两家 API 的最小客户端，带限流 / 5xx 退避重试，别的产品线直接复用
- `provider.TTLCache`（目录类查询缓存）、`provider.EncodeActionRef`（把地域 / 实例 / 动作编进
  异步句柄）、`provider.RegionIndex`（实例 ID → 地域，地域型 API 按 ID 操作时定位用）

参考资料（各家官方客户端，端点语义/分页/签名的"标准答案"）：
DigitalOcean [godo](https://github.com/digitalocean/godo)、
Vultr [govultr](https://github.com/vultr/govultr)（游标分页）、
腾讯云 [tencentcloud-sdk-go](https://github.com/TencentCloud/tencentcloud-sdk-go)、
阿里云 [aliyun-cli](https://github.com/aliyun/aliyun-cli)。

## 开发

```sh
make build / lint / test   # gofmt + go vet；go test -race
make check-ui              # 前端冒烟：DOM 桩 + 固定载荷完整执行页面脚本（需 node；pre-push 自动跑）
make hooks                 # 安装本地 pre-push 钩子：lint+test+check-ui 全绿才许 push，且禁止直推 main
```

协作流程：feature 分支 → 本地检查全绿 → push → GitHub PR → 人审合并（**无云端 CI**）。
