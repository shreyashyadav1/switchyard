// Package cache is a size-bounded LRU cache with per-entry expiry.
package cache

import (
	"container/list"
	"sync"
	"time"
)

// Entry is a cached upstream response.
type Entry struct {
	Body        []byte
	ContentType string
	Target      string
}

type item struct {
	key     string
	entry   Entry
	expires time.Time
}

// Cache holds at most max entries, evicting the least recently used.
type Cache struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	order *list.List
	items map[string]*list.Element
	now   func() time.Time
}

// New returns a cache using the given clock (time.Now when nil).
func New(ttl time.Duration, max int, now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{ttl: ttl, max: max, order: list.New(), items: map[string]*list.Element{}, now: now}
}

// Get returns the live entry for key.
func (c *Cache) Get(key string) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return Entry{}, false
	}
	it := el.Value.(*item)
	if !c.now().Before(it.expires) {
		c.order.Remove(el)
		delete(c.items, key)
		return Entry{}, false
	}
	c.order.MoveToFront(el)
	return it.entry, true
}

// Put stores e under key, evicting the oldest entries beyond the limit.
func (c *Cache) Put(key string, e Entry) {
	if c.max <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	expires := c.now().Add(c.ttl)
	if el, ok := c.items[key]; ok {
		it := el.Value.(*item)
		it.entry, it.expires = e, expires
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(&item{key: key, entry: e, expires: expires})
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*item).key)
	}
}

// Len returns the number of stored entries, including expired ones not yet
// evicted.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
