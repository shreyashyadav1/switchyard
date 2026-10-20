package ratelimit

import (
	"testing"
	"time"
)

func TestAllow(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(func() time.Time { return now })

	for i := range 3 {
		if ok, _ := l.Allow("a", 3); !ok {
			t.Fatalf("request %d should pass within the burst", i)
		}
	}
	ok, wait := l.Allow("a", 3)
	if ok {
		t.Fatal("fourth request should be limited")
	}
	if wait != 20*time.Second {
		t.Errorf("wait = %v, want 20s (3 rpm refills one token every 20s)", wait)
	}
	if ok, _ := l.Allow("b", 3); !ok {
		t.Error("keys must have separate buckets")
	}

	now = now.Add(20 * time.Second)
	if ok, _ := l.Allow("a", 3); !ok {
		t.Error("a token should have refilled after 20s")
	}
	if ok, _ := l.Allow("a", 3); ok {
		t.Error("only one token should have refilled")
	}

	now = now.Add(time.Hour)
	for i := range 3 {
		if ok, _ := l.Allow("a", 3); !ok {
			t.Fatalf("request %d: bucket should refill to capacity, not beyond", i)
		}
	}
	if ok, _ := l.Allow("a", 3); ok {
		t.Error("refill must be capped at capacity")
	}
}

func TestUnlimited(t *testing.T) {
	l := New(nil)
	for range 1000 {
		if ok, _ := l.Allow("a", 0); !ok {
			t.Fatal("rpm 0 means unlimited")
		}
	}
}
