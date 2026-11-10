package load

import (
	"testing"
	"time"
)

func TestSchedulerFixedRate(t *testing.T) {
	start := time.Unix(1000, 0)
	s := &Scheduler{Rate: 20, Start: start}
	var prev time.Time
	n := 0
	for i := 0; i < 21; i++ {
		at := s.Next()
		if want := start.Add(time.Duration(i) * 50 * time.Millisecond); !at.Equal(want) {
			t.Fatalf("slot %d = %v want %v", i, at, want)
		}
		if i > 0 && at.Sub(prev) != 50*time.Millisecond {
			t.Fatalf("gap %v", at.Sub(prev))
		}
		if at.Before(start.Add(time.Second)) {
			n++
		}
		prev = at
	}
	if n != 20 {
		t.Fatalf("slots within first second = %d, want 20", n)
	}
}

// After a stall, slots stay on the original schedule; overdue ones are
// returned immediately and the caller records the lag.
func TestSchedulerDoesNotBurstAfterStall(t *testing.T) {
	start := time.Unix(1000, 0)
	s := &Scheduler{Rate: 20, Start: start}
	s.Next() // slot 0
	// caller stalls 1s: 20 more slots are now overdue, but none is skipped or re-spaced
	for i := 1; i <= 25; i++ {
		at := s.Next()
		if want := start.Add(time.Duration(i) * 50 * time.Millisecond); !at.Equal(want) {
			t.Fatalf("slot %d = %v want %v", i, at, want)
		}
	}
	// lag for an overdue slot sent at stall end
	sentAt := start.Add(time.Second)
	if lag := LagMs(start.Add(50*time.Millisecond), sentAt); lag != 950 {
		t.Fatalf("lag = %d want 950", lag)
	}
}
