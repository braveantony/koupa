// Package load generates fixed-rate customer load.
package load

import (
	"sync"
	"time"
)

// Scheduler hands out send slots at a fixed rate: slot i is always exactly
// Start + i/Rate. It never skips or re-spaces slots. If the caller stalls,
// the overdue slots are returned immediately and the lag (sent - scheduled)
// is recorded by the caller, which avoids coordinated omission.
type Scheduler struct {
	Rate  float64
	Start time.Time

	mu sync.Mutex
	i  uint64
}

// Next returns the scheduled time of the next slot.
func (s *Scheduler) Next() time.Time {
	s.mu.Lock()
	i := s.i
	s.i++
	s.mu.Unlock()
	return s.Start.Add(time.Duration(float64(i) / s.Rate * float64(time.Second)))
}

// LagMs is the send lag in milliseconds (sent - scheduled), never negative.
func LagMs(scheduled, sent time.Time) int64 {
	if sent.Before(scheduled) {
		return 0
	}
	return sent.Sub(scheduled).Milliseconds()
}

// Wait sleeps until t or ctx is done; it returns immediately if t has passed.
func Wait(done <-chan struct{}, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return true
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return true
	case <-done:
		return false
	}
}
