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
// OrderID, Status and ReadySeq are pointers: nil (omitted) means not observed,
// which is different from an observed 0.
type Attempt struct {
	Run          string    `json:"run"`
	Kind         string    `json:"kind"`
	ClientRef    string    `json:"client_ref"`
	OrderID      *int64    `json:"order_id,omitempty"`
	Attempt      string    `json:"attempt"`
	Try          int       `json:"try"`
	ScheduledAt  time.Time `json:"scheduled_at"`
	SentAt       time.Time `json:"sent_at"`
	RespondedAt  time.Time `json:"responded_at"`
	Status       *int      `json:"status,omitempty"`
	ErrKind      string    `json:"err_kind"`
	PodUID       string    `json:"pod_uid"`
	ReadySeq     *int64    `json:"ready_seq,omitempty"`
	SchedLagMs   int64     `json:"sched_lag_ms"`
	FullResponse bool      `json:"full_response"`
}

// Writer writes one JSON object per line, safe for concurrent use. Each
// record is written with a single Write call and, if the destination
// implements Flush() error or Sync() error, flushed right after. It is meant
// for an *os.File. The first error is sticky and available from Err; the run
// must treat the measurement as invalid when Err() != nil.
type Writer struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

// NewWriter wraps w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Err returns the first write/flush error, if any.
func (w *Writer) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Write appends a. A failure (including a short write) is returned and kept.
func (w *Writer) Write(a Attempt) error {
	b, err := json.Marshal(a)
	if err != nil {
		return w.fail(err)
	}
	b = append(b, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.w.Write(b)
	if err == nil && n < len(b) {
		err = io.ErrShortWrite
	}
	if err == nil {
		switch f := w.w.(type) {
		case interface{ Flush() error }:
			err = f.Flush()
		case interface{ Sync() error }:
			err = f.Sync()
		}
	}
	if err != nil && w.err == nil {
		w.err = err
	}
	return err
}

func (w *Writer) fail(err error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = err
	}
	return err
}
