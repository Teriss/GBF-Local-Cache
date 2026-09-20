package cache

import (
	"container/list"
	"sync"
)

type ramItem struct {
	key   string
	entry CacheEntry
	body  []byte
	bytes int64
}

type RAMCache struct {
	mu        sync.Mutex
	maxBytes  int64
	maxObject int64
	usedBytes int64
	items     map[string]*list.Element
	order     *list.List
}

func NewRAMCache(maxBytes, maxObject int64) *RAMCache {
	if maxBytes < 1 {
		maxBytes = 256 << 20
	}
	if maxObject < 1 {
		maxObject = 8 << 20
	}
	return &RAMCache{
		maxBytes:  maxBytes,
		maxObject: maxObject,
		items:     make(map[string]*list.Element),
		order:     list.New(),
	}
}

func (c *RAMCache) Get(key string) (CacheEntry, []byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.items[key]
	if !ok {
		return CacheEntry{}, nil, false
	}
	c.order.MoveToFront(element)
	item := element.Value.(*ramItem)
	return item.entry.Clone(), append([]byte(nil), item.body...), true
}

func (c *RAMCache) Put(key string, entry CacheEntry, body []byte) bool {
	if int64(len(body)) > c.maxObject || int64(len(body)) > c.maxBytes {
		return false
	}
	bodyCopy := append([]byte(nil), body...)
	entry.ContentLength = int64(len(bodyCopy))

	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.items[key]; ok {
		c.usedBytes -= old.Value.(*ramItem).bytes
		c.order.Remove(old)
		delete(c.items, key)
	}
	item := &ramItem{key: key, entry: entry.Clone(), body: bodyCopy, bytes: int64(len(bodyCopy))}
	element := c.order.PushFront(item)
	c.items[key] = element
	c.usedBytes += item.bytes
	for c.usedBytes > c.maxBytes {
		last := c.order.Back()
		if last == nil {
			break
		}
		removed := last.Value.(*ramItem)
		c.usedBytes -= removed.bytes
		delete(c.items, removed.key)
		c.order.Remove(last)
	}
	return true
}

func (c *RAMCache) UsedBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usedBytes
}

func (c *RAMCache) MaxBytes() int64 {
	return c.maxBytes
}

func (c *RAMCache) Clear() {
	c.mu.Lock()
	c.items = make(map[string]*list.Element)
	c.order.Init()
	c.usedBytes = 0
	c.mu.Unlock()
}
