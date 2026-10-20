package cache

import (
	"testing"
	"time"
)

func TestCacheExpiryAndEviction(t *testing.T) {
	now := time.Unix(0, 0)
	c := New(time.Minute, 2, func() time.Time { return now })

	c.Put("a", Entry{Body: []byte("A")})
	c.Put("b", Entry{Body: []byte("B")})
	if _, ok := c.Get("a"); !ok { // a is now most recently used
		t.Fatal("a should be cached")
	}
	c.Put("c", Entry{Body: []byte("C")}) // evicts b
	if _, ok := c.Get("b"); ok {
		t.Error("b should have been evicted as least recently used")
	}
	if e, ok := c.Get("a"); !ok || string(e.Body) != "A" {
		t.Error("a should survive eviction")
	}

	now = now.Add(time.Minute)
	if _, ok := c.Get("a"); ok {
		t.Error("entries expire after the TTL")
	}
	if c.Len() != 1 {
		t.Errorf("expired entry should be removed on read, len = %d", c.Len())
	}

	c.Put("c", Entry{Body: []byte("C2")})
	if e, _ := c.Get("c"); string(e.Body) != "C2" {
		t.Error("Put should overwrite and refresh an existing key")
	}
}

func TestZeroSizeCacheStoresNothing(t *testing.T) {
	c := New(time.Minute, 0, nil)
	c.Put("a", Entry{})
	if _, ok := c.Get("a"); ok {
		t.Error("max 0 stores nothing")
	}
}
