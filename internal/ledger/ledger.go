// Package ledger tracks each key's spend for the current UTC day.
package ledger

import (
	"sync"
	"time"
)

// Ledger is an in-memory spend counter that resets at midnight UTC. It is
// per process: with several gateway replicas, each enforces budgets on its
// own share of the traffic.
type Ledger struct {
	mu    sync.Mutex
	day   string
	spent map[string]float64
	now   func() time.Time
}

// New returns a Ledger using the given clock (time.Now when nil).
func New(now func() time.Time) *Ledger {
	if now == nil {
		now = time.Now
	}
	return &Ledger{spent: map[string]float64{}, now: now}
}

func (l *Ledger) roll() {
	if day := l.now().UTC().Format(time.DateOnly); day != l.day {
		l.day = day
		clear(l.spent)
	}
}

// Spent returns key's spend today in US dollars.
func (l *Ledger) Spent(key string) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll()
	return l.spent[key]
}

// Add records usd against key for today.
func (l *Ledger) Add(key string, usd float64) {
	if usd <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll()
	l.spent[key] += usd
}
