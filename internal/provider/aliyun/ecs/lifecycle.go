package ecs

// lifecycle.go：电源、重装、改配、销毁，以及异步动作的状态判定。
//
// 阿里云这批接口都不返回任务号（StopInstance / StartInstance 只有 RequestId），
// ActionStatus 只能靠回读实例状态判定：
//   - 完成：实例到了目标状态（开机 / 重装 → 运行中；关机 → 已关机），改配是
//     同步接口，看规格是否已变成目标规格。
//   - 其余：进行中。平台侧的异步失败没有可读的错误信息，只能等编排层超时报错。
//
// 句柄里编着地域——编排层轮询时只回传句柄，而阿里云的实例接口必须打地域专属
// 接入点。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hoshinojian/vpsctl/internal/provider"
	"github.com/hoshinojian/vpsctl/internal/provider/aliyun"
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
// 关机用"普通停机模式"（KeepCharging）：按量实例停下后保留公网 IP 等资源，纳管
// 用的 IP 不变。优雅关机（shutdown）不强关——编排层靠"关机未完成就不删"兜底，
// 平台替用户硬关机会让这层保护失效。
func (c *Client) Power(ctx context.Context, id, action string) (provider.ActionRef, error) {
	region, inst, err := c.lookup(ctx, id)
	if err != nil {
		return provider.ActionRef{}, err
	}
	switch action {
	case provider.PowerOff, provider.Shutdown:
		params := map[string]string{"RegionId": region, "InstanceId": id, "ForceStop": "true"}
		kind := kindPowerOff
		if action == provider.Shutdown {
			params["ForceStop"] = "false"
			kind = kindShutdown
		}
		if inst.InstanceChargeType == postpaid {
			params["StoppedMode"] = "KeepCharging"
		}
		err = c.call(ctx, region, "StopInstance", params, nil)
		return provider.EncodeActionRef(region, id, kind, ""), err
	case provider.PowerOn:
		err = c.call(ctx, region, "StartInstance", map[string]string{"RegionId": region, "InstanceId": id}, nil)
		return provider.EncodeActionRef(region, id, kindPowerOn, ""), err
	default:
		return provider.ActionRef{}, fmt.Errorf("aliyun-ecs: 不支持的动作 %q", action)
	}
}

// ActionStatus 返回 in-progress / completed。
func (c *Client) ActionStatus(ctx context.Context, ref provider.ActionRef) (string, error) {
	_, id, kind, extra, err := provider.DecodeActionRef(ref.ID)
	if err != nil {
		return "", fmt.Errorf("aliyun-ecs: %w", err)
	}
	_, inst, err := c.lookup(ctx, id)
	if err != nil {
		return "", err
	}
	switch kind {
	case kindPowerOn, kindRebuild:
		if inst.Status == "Running" {
			return "completed", nil
		}
	case kindPowerOff, kindShutdown:
		if inst.Status == "Stopped" {
			return "completed", nil
		}
	case kindResize:
		if inst.InstanceType == extra {
			return "completed", nil
		}
	default:
		return "", fmt.Errorf("aliyun-ecs: 无法识别的动作种类 %q", kind)
	}
	return "in-progress", nil
}

// Rebuild 更换系统盘重装：保留实例 ID / 公网 IP，磁盘数据清空、不可恢复。
//
// 换盘要求实例处于已停止状态，换完实例停在已停止状态，所以链路是
// 运行中则先关机 → 换盘并等它结束 → 开机，返回的是最后一步开机的句柄。
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
	if inst.Status != "Stopped" {
		// 磁盘马上要被换掉，不需要优雅关机
		err = c.call(ctx, region, "StopInstance", map[string]string{"RegionId": region, "InstanceId": id, "ForceStop": "true"}, nil)
		if err != nil {
			return provider.ActionRef{}, fmt.Errorf("重装前关机失败，未重装: %w", err)
		}
		if err := c.waitInstance(ctx, id, func(i instance) (bool, error) {
			return i.Status == "Stopped", nil
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
	err = c.call(ctx, region, "ReplaceSystemDisk", map[string]string{"RegionId": region, "InstanceId": id, "ImageId": imageID}, nil)
	if err != nil {
		return provider.ActionRef{}, abort(fmt.Errorf("阿里云拒绝了重装（系统盘未动）: %w", err))
	}
	// 换盘是非即时操作：实例上挂着操作锁直到换完，镜像也在这时才变过去。
	// 两个条件都满足才开机——只看"没有锁"可能抢在锁登记之前。
	if err := c.waitInstance(ctx, id, func(i instance) (bool, error) {
		return len(i.OperationLocks.LockReason) == 0 && i.ImageId == imageID, nil
	}); err != nil {
		return provider.ActionRef{}, abort(fmt.Errorf("重装未完成: %w", err))
	}
	c.disks.Delete(region + "|" + id) // 系统盘已被换掉，缓存的盘信息作废
	if err := c.call(ctx, region, "StartInstance", map[string]string{"RegionId": region, "InstanceId": id}, nil); err != nil {
		return provider.ActionRef{}, fmt.Errorf("重装完成但开机失败（请手动开机）: %w", err)
	}
	return provider.EncodeActionRef(region, id, kindRebuild, ""), nil
}

// restorePower 是重装链路中途失败后的收拾：把为了重装才关掉的实例开回去，别让节点
// 悄悄停在关机状态。尽力而为——恢复的结果只追加在原始错误后面，不掩盖它；调用方
// 已取消时也照常发起，所以不用调用方的 ctx。
func (c *Client) restorePower(ctx context.Context, region, id string, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	err := c.call(ctx, region, "StartInstance", map[string]string{"RegionId": region, "InstanceId": id}, nil)
	switch {
	case err == nil:
		return fmt.Errorf("%w（已发起开机，节点恢复原状）", cause)
	case aliyun.IsCode(err, "InvalidInstanceId.NotFound"):
		return cause
	default:
		return fmt.Errorf("%w（恢复开机失败，节点现处于关机状态，请手动开机: %v）", cause, err)
	}
}

// Resize 调整规格（CPU / 内存），编排层保证实例已关机。接口是同步的，返回即
// 生效。阿里云的升配包不了磁盘扩容——要同时扩盘就直接报错，不悄悄只改一半。
func (c *Client) Resize(ctx context.Context, id, size string, resizeDisk bool) (provider.ActionRef, error) {
	if resizeDisk {
		return provider.ActionRef{}, errors.New("aliyun-ecs: 改配只调整规格（CPU/内存），不支持同时扩磁盘；需要更大磁盘请在控制台或用云盘扩容")
	}
	region, _, err := c.lookup(ctx, id)
	if err != nil {
		return provider.ActionRef{}, err
	}
	err = c.call(ctx, region, "ModifyInstanceSpec", map[string]string{
		"RegionId": region, "InstanceId": id, "InstanceType": size, "ClientToken": clientToken(id + size),
	}, nil)
	return provider.EncodeActionRef(region, id, kindResize, size), err
}

// Delete 释放实例：按量实例直接释放并停止计费；已不存在视为成功（批量重试幂等）。
// 运行中的实例也直接释放（与 DO 删除运行中节点同语义），Force 即"允许释放
// 运行中实例"。
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
	err := c.call(ctx, region, "DeleteInstance", map[string]string{"RegionId": region, "InstanceId": id, "Force": "true"}, nil)
	if aliyun.IsCode(err, "InvalidInstanceId.NotFound") {
		return nil
	}
	return err
}

// waitInstance 轮询实例直到 done 返回"完成"（此时它的 error 一并返回），超时或
// 取消报错。
func (c *Client) waitInstance(ctx context.Context, id string, done func(instance) (bool, error)) error {
	deadline := time.Now().Add(c.opWait)
	for {
		_, inst, err := c.lookup(ctx, id)
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
