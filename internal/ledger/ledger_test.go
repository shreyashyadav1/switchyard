package ledger

import (
	"math"
	"testing"
	"time"
)

func TestLedgerResetsAtMidnightUTC(t *testing.T) {
	now := time.Date(2026, 10, 20, 23, 59, 0, 0, time.UTC)
	l := New(func() time.Time { return now })

	l.Add("a", 0.25)
	l.Add("a", 0.5)
	l.Add("b", 1)
	l.Add("a", -3) // ignored
	if got := l.Spent("a"); math.Abs(got-0.75) > 1e-12 {
		t.Errorf("a spent %v, want 0.75", got)
	}

	now = now.Add(2 * time.Minute)
	if got := l.Spent("a"); got != 0 {
		t.Errorf("spend should reset at midnight UTC, got %v", got)
	}
	if got := l.Spent("b"); got != 0 {
		t.Errorf("every key resets, got %v", got)
	}
}
