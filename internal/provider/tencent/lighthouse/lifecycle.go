package lighthouse

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
	"github.com/hoshinojian/vpsctl/internal/provider/tencent"
)

// create.go + lifecycle.go：建机、电源、重装、升配、销毁。
//
// 这批接口都是异步的，却不返回任务号。进度只能看实例上的 LatestOperation /
// LatestOperationState——而且它们可能是上一轮操作留下的旧值。句柄里记着"做了什么"，
// ActionStatus 按下面的规则判定：
//   - 完成：实例到了目标状态且没有操作在执行；升配看套餐是否已变成目标套餐
//   - 失败：只认属于本动作的操作名报了失败
//   - 其余：进行中

// 动作种类，编码在 ActionRef.ID 的第三段。
const (
	kindPowerOn  = "power_on"
	kindPowerOff = "power_off"
	kindShutdown = "shutdown"
	kindRebuild  = "rebuild"
	kindResize   = "resize"
)

const (
	createVisibleWait     = 30 * time.Second
	createVisibleInterval = time.Second
	// initCommandTimeout 给"打开 root 登录"的命令留足时间：它要等默认用户的密码落盘，
	// 最多等 3 分钟。
	initCommandTimeout = 240
)

// rootLoginScript 是建机后由平台自动执行的命令（以 root 运行）。平台的"登录密码"设在
// 镜像的默认用户上（Ubuntu 是 ubuntu），root 默认禁用 SSH 登录；这里等默认用户的密码
// 落盘后把密码**哈希**原样复制给 root——命令内容会留在平台的命令记录里，所以绝不能
// 让明文密码出现在里面。纯 ASCII：不依赖镜像的语言环境。
//
// sshd 对每条指令取先读到的值，sshd_config.d 的分片又排在主配置之前：分片用 00- 前缀
// 保证最先被读到——Ubuntu 云镜像自带的 50-cloud-init.conf / 60-cloudimg-settings.conf
// 里是 PasswordAuthentication no，排在它们后面（99-）会被压住。
const rootLoginScript = `#!/bin/bash
# vpsctl: allow root to log in with the password the platform set on the image's default user.
hash=""
for _ in $(seq 1 90); do
  hash=$(awk -F: '$3 >= 1000 && $2 ~ /^\$/ {print $2; exit}' /etc/shadow)
  [ -n "$hash" ] && break
  sleep 2
done
if awk -F: '$1 == "root" && ($2 == "" || $2 ~ /^[!*]/) {found = 1} END {exit !found}' /etc/shadow; then
  if [ -z "$hash" ]; then
    echo "vpsctl: no default-user password found, root password not set" >&2
    exit 1
  fi
  usermod -p "$hash" root
fi
if [ -d /etc/ssh/sshd_config.d ]; then
  printf 'PermitRootLogin yes\nPasswordAuthentication yes\n' > /etc/ssh/sshd_config.d/00-vpsctl-root.conf
elif grep -qE '^[[:space:]]*PermitRootLogin' /etc/ssh/sshd_config; then
  sed -i -E 's/^[[:space:]]*PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
else
  printf '\nPermitRootLogin yes\n' >> /etc/ssh/sshd_config
fi
systemctl restart ssh 2>/dev/null || systemctl restart sshd 2>/dev/null || service ssh restart 2>/dev/null || true
`

// checkProvision 拒绝轻量应用服务器做不到的置备：它没有 cloud-init 用户数据通道，
// 靠 cloud-init 落地的那些能力在这里无处安放，宁可建机前说清楚。
func checkProvision(req provider.CreateRequest) error {
	switch {
	case req.SSHPort != 0 && req.SSHPort != 22:
		return errors.New("tencent-lighthouse: 不支持自定义 SSH 端口（轻量应用服务器没有 cloud-init 用户数据通道）")
	case req.Tunnel443:
		return errors.New("tencent-lighthouse: 不支持 443 隧道（轻量应用服务器没有 cloud-init 用户数据通道）")
	case len(req.AuthorizedKeys) > 0:
		return errors.New("tencent-lighthouse: 不支持注入运维公钥（轻量应用服务器没有 cloud-init 用户数据通道；登录密钥请用 --ssh-keys）")
	case req.UserData != "" && req.Login.Password == "":
		return errors.New("tencent-lighthouse: 不支持自定义 user-data（轻量应用服务器没有 cloud-init 用户数据通道；密码用缺省的随机密码，登录密钥用 --ssh-keys）")
	case req.Login.User != "" && req.Login.User != "root":
		return fmt.Errorf("tencent-lighthouse: 只支持给 root 注入密码，账号配置的 ssh_user 是 %q", req.Login.User)
	case len(req.SSHKeys) > 0 && req.Login.Password != "":
		return errors.New("tencent-lighthouse: 登录密钥与密码在平台上互斥，不能同时给——去掉 --ssh-keys 用随机密码，或加 --no-password 只用密钥")
	}
	return nil
}

// buildCreateParams 组装一次建机请求。dry 为 true 时带 DryRun（不落地的预检），否则带
// 幂等令牌。
func (c *Client) buildCreateParams(ctx context.Context, req provider.CreateRequest, dry bool) (region string, params map[string]any, err error) {
	region, zone, err := c.resolvePlacement(ctx, req.Region)
	if err != nil {
		return "", nil, err
	}
	blueprintID, err := c.blueprintID(ctx, region, req.Image)
	if err != nil {
		return "", nil, fmt.Errorf("解析镜像: %w", err)
	}
	if !strings.HasPrefix(req.Size, "bundle_") {
		return "", nil, fmt.Errorf("套餐 %q 不是轻量应用服务器的套餐 ID（可用 vpsctl sizes 查询，形如 bundle_starter_mc_med2_01）", req.Size)
	}
	params = map[string]any{
		"BundleId":    req.Size,
		"BlueprintId": blueprintID,
		// 建机即付一个月、到期自动续费
		"InstanceChargePrepaid": map[string]any{"Period": prepaidPeriod, "RenewFlag": autoRenewFlag},
	}
	if req.Name != "" {
		params["InstanceName"] = req.Name
	}
	if zone != "" {
		params["Zones"] = []string{zone}
	}
	if len(req.SSHKeys) > 0 {
		keys, err := c.resolveKeys(ctx, region, req.SSHKeys)
		if err != nil {
			return "", nil, err
		}
		params["LoginConfiguration"] = map[string]any{"KeyIds": keys}
	} else if req.Login.Password != "" {
		// 平台默认自动生成密码，要自定义就必须同时声明"不自动生成"
		params["LoginConfiguration"] = map[string]any{"AutoGeneratePassword": "NO", "Password": req.Login.Password}
		// 密码落在镜像默认用户上，root 默认禁登：让平台在开机后替我们把 root 打开
		params["InitCommand"] = map[string]any{
			"Content": base64.StdEncoding.EncodeToString([]byte(rootLoginScript)),
			"Timeout": initCommandTimeout,
		}
	}
	if tags := tencentTags(req.Tags); len(tags) > 0 {
		params["Tags"] = tags
	}
	if dry {
		params["DryRun"] = true
	} else {
		params["ClientToken"] = tencent.ClientToken(req.Name)
	}
	return region, params, nil
}

// Precheck 建机前的不落地预检（平台 DryRun）：参数、库存、余额的问题在扣费前报出来。
func (c *Client) Precheck(ctx context.Context, req provider.CreateRequest) error {
	if err := checkProvision(req); err != nil {
		return err
	}
	region, params, err := c.buildCreateParams(ctx, req, true)
	if err != nil {
		return err
	}
	if err := c.api.Call(ctx, host, service, version, "CreateInstances", region, params, nil); err != nil {
		return fmt.Errorf("建机预检（DryRun）未通过: %w", err)
	}
	return nil
}

// Create 提交建机并立即返回（实例随后进入 new → active）。建一次就付一个月，所以先
// 发一遍 DryRun，通过了才真建。
func (c *Client) Create(ctx context.Context, req provider.CreateRequest) (provider.Server, error) {
	if err := c.Precheck(ctx, req); err != nil {
		return provider.Server{}, err
	}
	region, params, err := c.buildCreateParams(ctx, req, false)
	if err != nil {
		return provider.Server{}, err
	}
	var out struct {
		InstanceIdSet []string `json:"InstanceIdSet"`
	}
	if err := c.api.Call(ctx, host, service, version, "CreateInstances", region, params, &out); err != nil {
		return provider.Server{}, err
	}
	if len(out.InstanceIdSet) != 1 {
		return provider.Server{}, fmt.Errorf("CreateInstances 返回 %d 个实例 ID，期望 1 个: %v", len(out.InstanceIdSet), out.InstanceIdSet)
	}
	id := out.InstanceIdSet[0]
	c.index.Put(id, region)
	return c.awaitVisible(ctx, region, id)
}

// awaitVisible 等刚建好的实例能查到（建机接口返回时实例还在落库，立即可查并不保证）。
func (c *Client) awaitVisible(ctx context.Context, region, id string) (provider.Server, error) {
	deadline := time.Now().Add(createVisibleWait)
	for {
		s, err := c.Get(ctx, id)
		if err == nil {
			return s, nil
		}
		if ctx.Err() != nil {
			return provider.Server{}, ctx.Err()
		}
		if time.Now().After(deadline) {
			return provider.Server{}, fmt.Errorf("实例 %s 已创建（地域 %s）但 %v 内查不到: %w", id, region, createVisibleWait, err)
		}
		if err := c.sleep(ctx, createVisibleInterval); err != nil {
			return provider.Server{}, err
		}
	}
}

// Power 发起电源动作，异步返回句柄。关机用平台的默认方式（先软关机，不行再硬关机）：
// 编排层靠"关机未完成就不删"兜底，不替用户硬断电。
func (c *Client) Power(ctx context.Context, id, action string) (provider.ActionRef, error) {
	region, _, err := c.lookup(ctx, id)
	if err != nil {
		return provider.ActionRef{}, err
	}
	var act, kind string
	switch action {
	case provider.PowerOff, provider.Shutdown:
		act, kind = "StopInstances", kindPowerOff
		if action == provider.Shutdown {
			kind = kindShutdown
		}
	case provider.PowerOn:
		act, kind = "StartInstances", kindPowerOn
	default:
		return provider.ActionRef{}, fmt.Errorf("tencent-lighthouse: 不支持的动作 %q", action)
	}
	err = c.powerAction(ctx, region, act, map[string]any{"InstanceIds": []string{id}})
	return provider.EncodeActionRef(region, id, kind, ""), err
}

// powerAction 下发电源动作，并吸收平台"实例上还有操作没收尾"的回应：等一会儿重试，
// 直到成功或等满 opWait（云服务器上实测改配刚"完成"就立刻开机会被这样拒绝）。
func (c *Client) powerAction(ctx context.Context, region, action string, params map[string]any) error {
	deadline := time.Now().Add(c.opWait)
	for {
		err := c.api.Call(ctx, host, service, version, action, region, params, nil)
		switch {
		case err == nil:
			return nil
		case action == "StartInstances" && tencent.IsCode(err, "UnsupportedOperation.InstanceStateRunning"):
			return nil // 已经在运行：开机本就是幂等的
		case action == "StopInstances" && tencent.IsCode(err, "UnsupportedOperation.InstanceStateStopped"):
			return nil
		case !tencent.IsCode(err, "OperationDenied.InstanceOperationInProgress"):
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("实例上的操作迟迟没有收尾（等了 %v）: %w", c.opWait, err)
		}
		if err := c.sleep(ctx, c.pollEvery); err != nil {
			return err
		}
	}
}

// operating 判定实例上是否有操作还在执行。
func operating(inst instance) bool {
	s := strings.ToUpper(inst.LatestOperationState)
	return strings.Contains(s, "OPERATING") || strings.Contains(s, "RUNNING")
}

// failed 判定本动作对应的操作是否已失败。只认属于本动作的操作名——字段里可能
// 还是上一轮操作留下的旧结果。
func failed(inst instance, ops ...string) error {
	s := strings.ToUpper(inst.LatestOperationState)
	if !strings.Contains(s, "FAILED") && !strings.Contains(s, "UNSUPPORTED") {
		return nil
	}
	if !slices.Contains(ops, inst.LatestOperation) {
		return nil
	}
	msg := inst.LatestOperationErrorMsg
	if msg == "" {
		msg = inst.LatestOperationState
	}
	return fmt.Errorf("腾讯云报操作 %s 失败: %s", inst.LatestOperation, msg)
}

// ActionStatus 返回 in-progress / completed / errored。
func (c *Client) ActionStatus(ctx context.Context, ref provider.ActionRef) (string, error) {
	region, id, kind, extra, err := provider.DecodeActionRef(ref.ID)
	if err != nil {
		return "", fmt.Errorf("tencent-lighthouse: %w", err)
	}
	inst, err := c.describe(ctx, region, id)
	if err != nil {
		return "", err
	}
	var opErr error
	switch kind {
	case kindPowerOn, kindRebuild:
		if inst.InstanceState == "RUNNING" && !operating(inst) {
			return "completed", nil
		}
		opErr = failed(inst, "StartInstances", "ResetInstance")
	case kindPowerOff, kindShutdown:
		if inst.InstanceState == "STOPPED" && !operating(inst) {
			return "completed", nil
		}
		opErr = failed(inst, "StopInstances")
	case kindResize:
		// 套餐已变、且这一轮操作已收尾才算完成
		if inst.BundleId == extra && !operating(inst) {
			return "completed", nil
		}
		opErr = failed(inst, "ModifyInstancesBundle")
	default:
		return "", fmt.Errorf("tencent-lighthouse: 无法识别的动作种类 %q", kind)
	}
	if opErr != nil {
		return "errored", opErr
	}
	return "in-progress", nil
}

// Rebuild 是 provider.Provider 的接口方法。轻量的重装必须重新下发登录密码（见
// RebuildWithLogin），这里直接拒绝，免得调用方绕开凭据检查把节点重装成登不进去的状态。
func (c *Client) Rebuild(_ context.Context, _, _ string) (provider.ActionRef, error) {
	return provider.ActionRef{}, errors.New("tencent-lighthouse: 重装必须重新下发登录密码，请走 RebuildWithLogin")
}

// RebuildWithLogin 重装系统盘：保留实例 ID / 公网 IP，磁盘数据清空、不可恢复。
//
// 重装的系统盘是全新的，平台的"登录密码"不会保留，所以要用 LoginConfiguration 把同一把
// 密码重新下发（与建机一样必须带 AutoGeneratePassword=NO，否则平台报参数冲突）。
//
// 只能重装到默认登录用户是 root 的镜像（见 rebuildableImage）：轻量没有 cloud-init /
// 初始化命令通道，Ubuntu 镜像重装后 root 拿不到密码。
//
// 重装要求实例处于关机状态，链路是 运行中则先关机 → 重装并等它结束 → 仍停着才开机。
// 重装被拒时把为了重装才关掉的实例开回去。
func (c *Client) RebuildWithLogin(ctx context.Context, id, image string, login provider.Login) (provider.ActionRef, error) {
	if err := rebuildableImage(image); err != nil {
		return provider.ActionRef{}, err
	}
	if login.Password == "" {
		return provider.ActionRef{}, errors.New("tencent-lighthouse: 重装必须带登录密码，否则节点重装后无法登录")
	}
	region, inst, err := c.lookup(ctx, id)
	if err != nil {
		return provider.ActionRef{}, err
	}
	blueprintID, err := c.blueprintID(ctx, region, image)
	if err != nil {
		return provider.ActionRef{}, fmt.Errorf("解析镜像: %w", err)
	}
	stopped := false
	if inst.InstanceState == "RUNNING" {
		err = c.powerAction(ctx, region, "StopInstances", map[string]any{"InstanceIds": []string{id}})
		if err != nil {
			return provider.ActionRef{}, fmt.Errorf("重装前关机失败，未重装: %w", err)
		}
		if err := c.waitInstance(ctx, region, id, func(i instance) (bool, error) {
			if err := failed(i, "StopInstances"); err != nil {
				return true, fmt.Errorf("重装前关机失败，未重装: %w", err)
			}
			return i.InstanceState == "STOPPED" && !operating(i), nil
		}); err != nil {
			return provider.ActionRef{}, fmt.Errorf("重装前未关成，未重装: %w", err)
		}
		stopped = true
	}
	abort := func(err error) error {
		if !stopped {
			return err
		}
		return c.restorePower(ctx, region, id, err)
	}
	err = c.api.Call(ctx, host, service, version, "ResetInstance", region, map[string]any{
		"InstanceId":         id,
		"BlueprintId":        blueprintID,
		"LoginConfiguration": map[string]any{"AutoGeneratePassword": "NO", "Password": login.Password},
	}, nil)
	if err != nil {
		return provider.ActionRef{}, abort(fmt.Errorf("腾讯云拒绝了重装（系统盘未动）: %w", err))
	}
	if err := c.waitInstance(ctx, region, id, func(i instance) (bool, error) {
		if err := failed(i, "ResetInstance"); err != nil {
			return true, err
		}
		return i.LatestOperation == "ResetInstance" && !operating(i), nil
	}); err != nil {
		return provider.ActionRef{}, abort(fmt.Errorf("重装未完成: %w", err))
	}
	cur, err := c.describe(ctx, region, id)
	if err != nil {
		return provider.ActionRef{}, fmt.Errorf("重装完成但查询实例状态失败（请确认实例是否已开机）: %w", err)
	}
	if cur.InstanceState == "STOPPED" {
		if err := c.powerAction(ctx, region, "StartInstances", map[string]any{"InstanceIds": []string{id}}); err != nil {
			return provider.ActionRef{}, fmt.Errorf("重装完成但开机失败（请手动开机）: %w", err)
		}
	}
	return provider.EncodeActionRef(region, id, kindRebuild, ""), nil
}

// rebuildableImage 判断重装到该镜像后节点还能不能用密码登进去。
//
// 平台的登录密码落在镜像的**默认用户**上（官方 FAQ：Linux 的默认用户是 root，Ubuntu 是
// ubuntu）。轻量没有 cloud-init 用户数据、也没有能重跑的初始化命令通道——建机时那条把
// 密码哈希搬给 root 的命令（InitCommand）只在创建时执行，ResetInstance 不会重跑它。所以
// Ubuntu 镜像重装后 root 拿不到密码、sshd 也不会放开 root 登录，节点必然登不进去。
//
// 实测（2026-11-25，真机）：debian-12 重装后 root 用同一把密码直接可登（镜像本身
// permitrootlogin / passwordauthentication 都是 yes）；ubuntu 系重装后 root 与 ubuntu
// 两个用户都拒绝该密码。
func rebuildableImage(slug string) error {
	if strings.HasPrefix(slug, "ubuntu") {
		return fmt.Errorf("tencent-lighthouse: %q 的默认登录用户是 ubuntu 而不是 root；轻量没有"+
			" cloud-init / 初始化命令通道，重装后把密码搬给 root 的那条命令不会重跑，节点会变得"+
			" root 登不进去。请改用 debian 等默认用户为 root 的镜像，或销毁后重建", slug)
	}
	return nil
}

// restorePower 是重装链路中途失败后的收拾：把为了重装才关掉的实例开回去。尽力而为——
// 恢复的结果只追加在原始错误后面，不掩盖它；调用方已取消时也照常发起。
func (c *Client) restorePower(ctx context.Context, region, id string, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	err := c.powerAction(ctx, region, "StartInstances", map[string]any{"InstanceIds": []string{id}})
	switch {
	case err == nil:
		return fmt.Errorf("%w（已发起开机，节点恢复原状）", cause)
	case tencent.IsCode(err, "ResourceNotFound.InstanceNotFound"):
		return cause
	default:
		return fmt.Errorf("%w（恢复开机失败，节点现处于关机状态，请手动开机: %v）", cause, err)
	}
}

// Resize 升配（轻量应用服务器只能升不能降）。编排层保证实例已关机；编排层随后会
// 自己开机。磁盘扩容同样不支持——要同时扩盘就直接报错，不悄悄只改一半。
func (c *Client) Resize(ctx context.Context, id, size string, resizeDisk bool) (provider.ActionRef, error) {
	if resizeDisk {
		return provider.ActionRef{}, errors.New("tencent-lighthouse: 升配不支持同时扩磁盘；要更大的盘选磁盘更大的套餐")
	}
	if !strings.HasPrefix(size, "bundle_") {
		return provider.ActionRef{}, fmt.Errorf("套餐 %q 不是轻量应用服务器的套餐 ID（可用 vpsctl sizes 查询）", size)
	}
	region, _, err := c.lookup(ctx, id)
	if err != nil {
		return provider.ActionRef{}, err
	}
	err = c.api.Call(ctx, host, service, version, "ModifyInstancesBundle", region, map[string]any{
		"InstanceIds": []string{id}, "BundleId": size,
	}, nil)
	return provider.EncodeActionRef(region, id, kindResize, size), err
}

// Delete 销毁实例；已不存在视为成功。
//
// 轻量应用服务器是包年包月产品，平台把"销毁"拆成两步：IsolateInstances 先把实例退还
// （进入 SHUTDOWN，退款流程随之启动），TerminateInstances 才真正销毁，而后者只接受
// SHUTDOWN 状态的实例。直接发 TerminateInstances 对运行中和已关机的实例都会报
// UnsupportedOperation.InvalidInstanceState——真机验证时踩到，即销毁接口原本完全不可用。
//
// 退还本身被平台拒绝（例如套餐不支持自助退还）时把平台原文带出来，不谎报成功。
func (c *Client) Delete(ctx context.Context, id string) error {
	region, ok := c.index.Get(id)
	if !ok {
		var err error
		if region, err = c.locate(ctx, id); err != nil {
			if errors.Is(err, errNotFound) {
				return nil
			}
			return err
		}
	}
	inst, err := c.describe(ctx, region, id)
	switch {
	case errors.Is(err, errNotFound):
		return nil
	case err != nil:
		return err
	}
	if inst.InstanceState != "SHUTDOWN" {
		err = c.api.Call(ctx, host, service, version, "IsolateInstances", region,
			map[string]any{"InstanceIds": []string{id}}, nil)
		switch {
		case tencent.IsCode(err, "ResourceNotFound.InstanceNotFound"):
			return nil
		case err != nil:
			return fmt.Errorf("退还实例失败（尚未销毁）: %w", err)
		}
		if err := c.waitInstance(ctx, region, id, func(i instance) (bool, error) {
			if err := failed(i, "IsolateInstances"); err != nil {
				return true, err
			}
			return i.InstanceState == "SHUTDOWN", nil
		}); err != nil {
			return fmt.Errorf("实例未进入 SHUTDOWN，未发起销毁: %w", err)
		}
	}
	err = c.api.Call(ctx, host, service, version, "TerminateInstances", region,
		map[string]any{"InstanceIds": []string{id}}, nil)
	if tencent.IsCode(err, "ResourceNotFound.InstanceNotFound") {
		return nil
	}
	return err
}

// waitInstance 轮询实例直到 done 返回"完成"（此时它的 error 一并返回），超时或取消报错。
func (c *Client) waitInstance(ctx context.Context, region, id string, done func(instance) (bool, error)) error {
	deadline := time.Now().Add(c.opWait)
	for {
		inst, err := c.describe(ctx, region, id)
		if err == nil {
			if finished, ferr := done(inst); finished {
				return ferr
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待实例 %s 状态就绪超时（>%v）", id, c.opWait)
		}
		if err := c.sleep(ctx, c.pollEvery); err != nil {
			return err
		}
	}
}
