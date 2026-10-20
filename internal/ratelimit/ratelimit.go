// Package ratelimit implements per-key token buckets.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter holds one token bucket per key. A bucket's capacity is the key's
// requests-per-minute limit and it refills continuously.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

// New returns a Limiter using the given clock (time.Now when nil).
func New(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{buckets: map[string]*bucket{}, now: now}
}

// Allow takes a token from key's bucket. When the bucket is empty it returns
// false and how long until the next token. rpm <= 0 means unlimited.
func (l *Limiter) Allow(key string, rpm int) (bool, time.Duration) {
	if rpm <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	capacity := float64(rpm)
	perSecond := capacity / 60
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: capacity, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(capacity, b.tokens+now.Sub(b.last).Seconds()*perSecond)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / perSecond * float64(time.Second))
	return false, wait
}
