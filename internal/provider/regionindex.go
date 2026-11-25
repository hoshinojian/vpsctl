package provider

import "sync"

// RegionIndex 记录资源 ID → 地域，供按 ID 操作时定位地域。
//
// 腾讯云、阿里云的实例操作接口都要求带上地域（腾讯云走 X-TC-Region，阿里云要打
// 地域专属接入点），而 Provider 接口的 Get/Power/Delete 等只拿实例 ID。实例 ID
// 全局唯一且不复用，所以 List / Create 时顺手登记即可；未登记过的 ID（如进程
// 刚启动就对某台机器直接发操作）由各实现在 ActionStatus 之外自行兜底——通常是
// 逐地域并发查找一次后再登记。
type RegionIndex struct {
	mu   sync.RWMutex
	byID map[string]string
}

// Put 登记实例 ID 所在地域；重复登记以后者为准。
func (r *RegionIndex) Put(id, region string) {
	if id == "" || region == "" {
		return
	}
	r.mu.Lock()
	if r.byID == nil {
		r.byID = make(map[string]string)
	}
	r.byID[id] = region
	r.mu.Unlock()
}

// Get 查实例 ID 所在地域；未登记返回空串与 false。
func (r *RegionIndex) Get(id string) (string, bool) {
	r.mu.RLock()
	region, ok := r.byID[id]
	r.mu.RUnlock()
	return region, ok
}
