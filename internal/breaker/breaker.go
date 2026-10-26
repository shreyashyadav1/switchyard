// Package breaker implements per-target circuit breakers, so a provider that
// keeps failing is skipped for a while instead of costing every request its
// retries before the fallback is tried.
package breaker

import (
	"sync"
	"time"
)

// State is a breaker's position.
type State int

const (
	// Closed lets requests through and counts consecutive failures.
	Closed State = iota
	// Open skips the target until the cooldown ends.
	Open
	// HalfOpen lets a single probe request through after the cooldown.
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Open:
		return "open"
	case HalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

type entry struct {
	state    State
	failures int
	openedAt time.Time
	probing  bool
}

// Set holds one breaker per target label.
type Set struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	entries   map[string]*entry
	now       func() time.Time
	// onChange is called, outside the lock, when a breaker changes state.
	onChange func(target string, from, to State)
}

// New returns a Set that opens a breaker after threshold consecutive failures
// and keeps it open for cooldown. It uses the given clock (time.Now when nil).
// onChange may be nil.
func New(threshold int, cooldown time.Duration, now func() time.Time, onChange func(target string, from, to State)) *Set {
	if now == nil {
		now = time.Now
	}
	if onChange == nil {
		onChange = func(string, State, State) {}
	}
	return &Set{threshold: threshold, cooldown: cooldown, entries: map[string]*entry{}, now: now, onChange: onChange}
}

// Allow reports whether a request may go to target. While the breaker is
// open it returns false and how long until a probe will be let through. Once
// the cooldown has passed, exactly one caller gets true (the probe) until it
// reports back with Success, Failure or Release.
func (s *Set) Allow(target string) (bool, time.Duration) {
	s.mu.Lock()
	e := s.entry(target)
	from := e.state
	switch e.state {
	case Closed:
		s.mu.Unlock()
		return true, 0
	case Open:
		wait := e.openedAt.Add(s.cooldown).Sub(s.now())
		if wait > 0 {
			s.mu.Unlock()
			return false, wait
		}
		e.state = HalfOpen
	}
	if e.probing {
		s.mu.Unlock()
		// Another request is probing; it reports back within one request timeout.
		return false, s.cooldown
	}
	e.probing = true
	s.mu.Unlock()
	if from != HalfOpen {
		s.onChange(target, from, HalfOpen)
	}
	return true, 0
}

// Success closes the breaker and clears its failure count.
func (s *Set) Success(target string) {
	s.mu.Lock()
	e := s.entry(target)
	from := e.state
	e.state, e.failures, e.probing = Closed, 0, false
	s.mu.Unlock()
	if from != Closed {
		s.onChange(target, from, Closed)
	}
}

// Failure counts a failed request. It opens the breaker when the failure
// count reaches the threshold, and reopens it when a probe fails.
func (s *Set) Failure(target string) {
	s.mu.Lock()
	e := s.entry(target)
	from := e.state
	e.failures++
	if e.state == HalfOpen || e.failures >= s.threshold {
		e.state, e.openedAt = Open, s.now()
	}
	e.probing = false
	to := e.state
	s.mu.Unlock()
	if from != to {
		s.onChange(target, from, to)
	}
}

// Release ends a request whose result says nothing about the target, such
// as one the client cancelled, so a pending probe slot is freed.
func (s *Set) Release(target string) {
	s.mu.Lock()
	s.entry(target).probing = false
	s.mu.Unlock()
}

// State returns target's current state.
func (s *Set) State(target string) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entry(target).state
}

func (s *Set) entry(target string) *entry {
	e, ok := s.entries[target]
	if !ok {
		e = &entry{}
		s.entries[target] = e
	}
	return e
}
