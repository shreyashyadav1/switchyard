package breaker

import (
	"sync"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newSet(c *clock, changes *[]string) *Set {
	return New(3, 30*time.Second, c.now, func(target string, from, to State) {
		*changes = append(*changes, target+":"+from.String()+"->"+to.String())
	})
}

func TestOpensAfterConsecutiveFailures(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var changes []string
	s := newSet(c, &changes)

	for range 2 {
		s.Failure("a")
	}
	if ok, _ := s.Allow("a"); !ok {
		t.Fatal("breaker opened before the threshold")
	}
	s.Failure("a")
	ok, wait := s.Allow("a")
	if ok || wait != 30*time.Second {
		t.Fatalf("Allow = %v, %v; want false, 30s", ok, wait)
	}
	if got := s.State("b"); got != Closed {
		t.Fatalf("other target is %v; breakers must be per target", got)
	}
	if len(changes) != 1 || changes[0] != "a:closed->open" {
		t.Fatalf("changes = %v", changes)
	}
}

func TestSuccessResetsFailureCount(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var changes []string
	s := newSet(c, &changes)
	s.Failure("a")
	s.Failure("a")
	s.Success("a")
	s.Failure("a")
	s.Failure("a")
	if got := s.State("a"); got != Closed {
		t.Fatalf("state = %v; failures must be consecutive", got)
	}
}

func TestHalfOpenLetsOneProbeThrough(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var changes []string
	s := newSet(c, &changes)
	for range 3 {
		s.Failure("a")
	}
	c.advance(10 * time.Second)
	if ok, wait := s.Allow("a"); ok || wait != 20*time.Second {
		t.Fatalf("Allow during cooldown = %v, %v; want false, 20s", ok, wait)
	}
	c.advance(20 * time.Second)
	if ok, _ := s.Allow("a"); !ok {
		t.Fatal("no probe after the cooldown")
	}
	if ok, _ := s.Allow("a"); ok {
		t.Fatal("a second request got through while the probe was in flight")
	}
	s.Success("a")
	if ok, _ := s.Allow("a"); !ok || s.State("a") != Closed {
		t.Fatal("a successful probe must close the breaker")
	}
	want := []string{"a:closed->open", "a:open->half_open", "a:half_open->closed"}
	if len(changes) != len(want) {
		t.Fatalf("changes = %v, want %v", changes, want)
	}
	for i := range want {
		if changes[i] != want[i] {
			t.Fatalf("changes = %v, want %v", changes, want)
		}
	}
}

func TestFailedProbeReopens(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var changes []string
	s := newSet(c, &changes)
	for range 3 {
		s.Failure("a")
	}
	c.advance(30 * time.Second)
	if ok, _ := s.Allow("a"); !ok {
		t.Fatal("no probe after the cooldown")
	}
	s.Failure("a")
	if ok, wait := s.Allow("a"); ok || wait != 30*time.Second {
		t.Fatalf("Allow after a failed probe = %v, %v; want a fresh 30s cooldown", ok, wait)
	}
}

func TestReleaseFreesTheProbe(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var changes []string
	s := newSet(c, &changes)
	for range 3 {
		s.Failure("a")
	}
	c.advance(30 * time.Second)
	s.Allow("a")
	s.Release("a")
	if ok, _ := s.Allow("a"); !ok {
		t.Fatal("a released probe must let the next request probe")
	}
	if got := s.State("a"); got != HalfOpen {
		t.Fatalf("state = %v, want half_open", got)
	}
}

func TestConcurrentUse(t *testing.T) {
	s := New(5, time.Minute, nil, nil)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := s.Allow("a"); ok {
				if i%2 == 0 {
					s.Failure("a")
				} else {
					s.Success("a")
				}
			}
		}()
	}
	wg.Wait()
}
