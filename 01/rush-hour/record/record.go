// Package record defines the Rush Hour per-attempt JSONL record and a
// goroutine-safe writer for it.
package record

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Record kinds.
const (
	KindOrder = "order"
	KindReady = "ready"
	KindProbe = "probe"
)

// Error kinds (empty when a response was received).
const (
	ErrRefused = "refused"
	ErrReset   = "reset"
	ErrTimeout = "timeout"
)

// Attempt is one JSONL line: one request attempt (retries get their own line).
type Attempt struct {
	Run          string    `json:"run"`
	Kind         string    `json:"kind"`
	ClientRef    string    `json:"client_ref"`
	OrderID      int64     `json:"order_id"`
	Attempt      string    `json:"attempt"`
	Try          int       `json:"try"`
	ScheduledAt  time.Time `json:"scheduled_at"`
	SentAt       time.Time `json:"sent_at"`
	RespondedAt  time.Time `json:"responded_at"`
	Status       int       `json:"status"`
	ErrKind      string    `json:"err_kind"`
	PodUID       string    `json:"pod_uid"`
	ReadySeq     int64     `json:"ready_seq"`
	SchedLagMs   int64     `json:"sched_lag_ms"`
	FullResponse bool      `json:"full_response"`
}

// Writer writes one JSON object per line, safe for concurrent use. Each
// record is written with a single Write call, so it is flushed immediately.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter wraps w. w is not buffered by the Writer; pass an unbuffered
// destination (e.g. *os.File) so every record reaches it at once.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Write appends a.
func (w *Writer) Write(a Attempt) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.w.Write(b)
	return err
}
