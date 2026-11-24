package cvm

// lifecycle.go：电源、重装、改配、销毁，以及异步动作的状态判定。
//
// 腾讯云这批接口都是异步的，却不返回任务号。能用来判定进度的只有实例上的
// LatestOperation / LatestOperationState 两个字段——而且它们可能是上一轮操作
// 留下的旧值。所以句柄里记清"做了什么"，ActionStatus 按下面的规则判定：
//   - 完成：实例到了目标状态（开机 / 重装 → 运行中；关机 → 已关机），且没有
//     操作在执行；改配看机型是否已变成目标机型。
//   - 失败：只认属于本动作的操作名报了失败，不给上一轮的旧失败"定罪"。
//   - 其余：进行中。

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
	"github.com/hoshinojian/vpsctl/internal/provider/tencent"
)

// 动作种类，编码在 ActionRef.ID 的第三段。
const (
	kindPowerOn  = "power_on"
	kindPowerOff = "power_off"
	kindShutdown = "shutdown"
	kindRebuild  = "rebuild"
	kindResize   = "resize"
)

// Power 发起电源动作，异步返回句柄。
//
// 关机用"关机不停计费"模式：按量实例停下后保留公网 IP 等资源，纳管用的 IP 不变。
// 优雅关机（shutdown）只用软关机，不落到硬断电——编排层靠"关机未完成就不删"
// 兜底，平台替用户硬关机会让这层保护失效。
func (c *Client) Power(ctx context.Context, id, action string) (provider.ActionRef, error) {
	region, inst, err := c.lookup(ctx, id)
	if err != nil {
		return provider.ActionRef{}, err
	}
	switch action {
	case provider.PowerOff, provider.Shutdown:
		params := map[string]any{"InstanceIds": []string{id}, "StopType": "HARD"}
		kind := kindPowerOff
		if action == provider.Shutdown {
			params["StopType"] = "SOFT"
			kind = kindShutdown
		}
		if inst.InstanceChargeType == postpaid {
			params["StoppedMode"] = "KEEP_CHARGING"
		}
		err = c.powerAction(ctx, region, "StopInstances", params)
		return provider.EncodeActionRef(region, id, kind, ""), err
	case provider.PowerOn:
		err = c.powerAction(ctx, region, "StartInstances", map[string]any{"InstanceIds": []string{id}})
		return provider.EncodeActionRef(region, id, kindPowerOn, ""), err
	default:
		return provider.ActionRef{}, fmt.Errorf("tencent-cvm: 不支持的动作 %q", action)
	}
}

// powerAction 下发电源动作，并吸收平台的两类"不必当失败"的回应：
//
//   - 目标状态已达成（开机时实例已在运行）：电源动作本就幂等，直接当成功；
//   - 实例上还有操作没收尾（改配刚完成时平台还要再忙一阵）：等一会儿重试，
//     直到成功或等满 opWait。
//
// 实测改配刚"完成"就立刻开机会被 OperationDenied.InstanceOperationInProgress
// 拒绝，不吸收的话节点会被留在关机状态。
func (c *Client) powerAction(ctx context.Context, region, action string, params map[string]any) error {
	deadline := time.Now().Add(c.opWait)
	for {
		err := c.api.Call(ctx, host, service, version, action, region, params, nil)
		switch {
		case err == nil:
			return nil
		case action == "StartInstances" && tencent.IsCode(err, "UnsupportedOperation.InstanceStateRunning"):
			return nil
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
	if msg == "" || msg == "None" {
		msg = inst.LatestOperationState
	}
	return fmt.Errorf("腾讯云报操作 %s 失败: %s", inst.LatestOperation, msg)
}

// ActionStatus 返回 in-progress / completed / errored。
func (c *Client) ActionStatus(ctx context.Context, ref provider.ActionRef) (string, error) {
	region, id, kind, extra, err := provider.DecodeActionRef(ref.ID)
	if err != nil {
		return "", err
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
		// 机型已变、且这一轮操作已收尾才算完成：机型先落、操作后收，收尾期间
		// 平台还会拒绝别的操作
		if inst.InstanceType == extra && !operating(inst) {
			return "completed", nil
		}
		opErr = failed(inst, "ResetInstancesType")
	default:
		return "", fmt.Errorf("tencent-cvm: 无法识别的动作种类 %q", kind)
	}
	if opErr != nil {
		return "errored", opErr
	}
	return "in-progress", nil
}

// Rebuild 重装系统盘：保留实例 ID / 公网 IP，磁盘数据清空、不可恢复。
//
// 重装要求实例处于关机状态，重装完实例停在关机状态，所以链路是
// 运行中则先关机 → 重装并等它结束 → 开机，返回的是最后一步开机的句柄。
func (c *Client) Rebuild(ctx context.Context, id, image string) (provider.ActionRef, error) {
	region, inst, err := c.lookup(ctx, id)
	if err != nil {
		return provider.ActionRef{}, err
	}
	imageID, err := c.imageID(ctx, region, image)
	if err != nil {
		return provider.ActionRef{}, fmt.Errorf("解析镜像: %w", err)
	}
	// stopped 记下"这台机是我们为了重装才关的"：重装没成就要把它开回去
	stopped := false
	if inst.InstanceState == "RUNNING" {
		err = c.powerAction(ctx, region, "StopInstances", map[string]any{
			"InstanceIds": []string{id}, "StopType": "HARD",
		})
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
		"InstanceId": id, "ImageId": imageID,
	}, nil)
	if err != nil {
		// 镜像不适配机型这类拒绝都落在这里，此时系统盘没动过
		return provider.ActionRef{}, abort(fmt.Errorf("腾讯云拒绝了重装（系统盘未动）: %w", err))
	}
	// 必须见到这次重装登记过并已结束才能开机：刚下发时字段里可能还是上一轮
	// 操作的"成功"，只看"没操作在跑"会抢在重装真正开始前开机。
	if err := c.waitInstance(ctx, region, id, func(i instance) (bool, error) {
		if err := failed(i, "ResetInstance"); err != nil {
			return true, err
		}
		return i.LatestOperation == "ResetInstance" && !operating(i), nil
	}); err != nil {
		return provider.ActionRef{}, abort(fmt.Errorf("重装未完成: %w", err))
	}
	// 重装完平台会把实例自动开起来（实测）；只有仍停在关机状态时才需要我们开机
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

// restorePower 是重装链路中途失败后的收拾：把为了重装才关掉的实例开回去，别让节点
// 悄悄停在关机状态。尽力而为——恢复的结果只追加在原始错误后面，不掩盖它；调用方
// 已取消时也照常发起，所以不用调用方的 ctx。
func (c *Client) restorePower(ctx context.Context, region, id string, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	err := c.powerAction(ctx, region, "StartInstances", map[string]any{"InstanceIds": []string{id}})
	switch {
	case err == nil:
		return fmt.Errorf("%w（已发起开机，节点恢复原状）", cause)
	case tencent.IsCode(err, "InvalidInstanceId.NotFound"):
		return cause
	default:
		return fmt.Errorf("%w（恢复开机失败，节点现处于关机状态，请手动开机: %v）", cause, err)
	}
}

// Resize 调整机型（CPU / 内存），编排层保证实例已关机。腾讯云的改配不含磁盘
// 扩容——要同时扩盘就直接报错，不悄悄只改一半。
func (c *Client) Resize(ctx context.Context, id, size string, resizeDisk bool) (provider.ActionRef, error) {
	if resizeDisk {
		return provider.ActionRef{}, errors.New("tencent-cvm: 改配只调整机型（CPU/内存），不支持同时扩磁盘；需要更大磁盘请在控制台或用 CBS 扩容系统盘")
	}
	region, _, err := c.lookup(ctx, id)
	if err != nil {
		return provider.ActionRef{}, err
	}
	err = c.api.Call(ctx, host, service, version, "ResetInstancesType", region, map[string]any{
		"InstanceIds": []string{id}, "InstanceType": size,
	}, nil)
	return provider.EncodeActionRef(region, id, kindResize, size), err
}

// Delete 销毁实例：按量实例直接销毁并停止计费；已不存在视为成功（批量重试幂等）。
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
	err := c.api.Call(ctx, host, service, version, "TerminateInstances", region,
		map[string]any{"InstanceIds": []string{id}}, nil)
	if tencent.IsCode(err, "InvalidInstanceId.NotFound") {
		return nil
	}
	return err
}

// waitInstance 轮询实例直到 done 返回"完成"（此时它的 error 一并返回），超时或
// 取消报错。
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
