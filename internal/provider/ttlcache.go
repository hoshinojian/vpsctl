package provider

import (
	"sync"
	"time"
)

// TTLCache 是按 key 缓存目录类查询结果的小缓存——镜像、机型、询价这类"基本不变
// 但每次列表都要用"的数据。地域型提供商（腾讯云、阿里云）按地域各存一份，并发安全。
type TTLCache[T any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]ttlEntry[T]
}

type ttlEntry[T any] struct {
	v  T
	at time.Time
}

func NewTTLCache[T any](ttl time.Duration) *TTLCache[T] {
	return &TTLCache[T]{ttl: ttl, now: time.Now, entries: map[string]ttlEntry[T]{}}
}

// Get 取未过期的缓存值。
func (c *TTLCache[T]) Get(key string) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || c.now().Sub(e.at) > c.ttl {
		var zero T
		return zero, false
	}
	return e.v, true
}

// Put 写入缓存值。
func (c *TTLCache[T]) Put(key string, v T) {
	c.mu.Lock()
	c.entries[key] = ttlEntry[T]{v, c.now()}
	c.mu.Unlock()
}

// Delete 使一个缓存值作废（底层的资源被换掉、重建过之后调用）。
func (c *TTLCache[T]) Delete(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}
