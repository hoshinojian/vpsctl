package provider

import (
	"fmt"
	"strings"
)

// HoursPerMonth 把按量小时单价折成月费（与 DO 的 price_monthly 同量纲的估算值）。
const HoursPerMonth = 730.0

// EncodeActionRef 把"地域|实例 ID|动作|附加信息"编成异步动作句柄。
//
// 地域型提供商的按 ID 接口都要带地域，而编排层轮询时只回传句柄，所以地域必须
// 编进去；腾讯云、阿里云的异步接口又没有统一的任务号，动作种类用来告诉
// ActionStatus 该等什么状态，附加信息按需携带（改配放目标机型 / 规格）。
func EncodeActionRef(region, id, kind, extra string) ActionRef {
	return ActionRef{ID: region + "|" + id + "|" + kind + "|" + extra}
}

// DecodeActionRef 是 EncodeActionRef 的逆运算；句柄不完整时报错。
func DecodeActionRef(s string) (region, id, kind, extra string, err error) {
	parts := strings.Split(s, "|")
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" {
		return "", "", "", "", fmt.Errorf("无法识别的动作句柄 %q", s)
	}
	return parts[0], parts[1], parts[2], parts[3], nil
}
