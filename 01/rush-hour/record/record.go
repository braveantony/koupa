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
type Writer struct{ lw lineWriter }

// NewWriter wraps w.
func NewWriter(w io.Writer) *Writer { return &Writer{lw: lineWriter{w: w}} }

// Err returns the first write/flush error, if any.
func (w *Writer) Err() error { return w.lw.Err() }

// Write appends a. A failure (including a short write) is returned and kept.
func (w *Writer) Write(a Attempt) error { return w.lw.write(a) }

// Board event kinds.
const (
	BoardConnect    = "connect"
	BoardHello      = "hello"
	BoardReady      = "ready"
	BoardShutdown   = "shutdown"
	BoardDisconnect = "disconnect"
	BoardError      = "error"
)

// BoardEvent is one JSONL line of a board's event log. Pointer fields are nil
// (omitted) when the value was not observed, which differs from an observed 0.
type BoardEvent struct {
	Run          string    `json:"run"`
	Board        string    `json:"board"`
	Conn         int       `json:"conn"`
	Kind         string    `json:"kind"`
	At           time.Time `json:"at"`
	PodUID       *string   `json:"pod_uid,omitempty"`
	Start        *int64    `json:"start,omitempty"`
	Version      *string   `json:"version,omitempty"`
	ReadySeq     *int64    `json:"ready_seq,omitempty"`
	Number       *string   `json:"number,omitempty"`
	ShutdownSeen *bool     `json:"shutdown_seen,omitempty"`
	Status       *int      `json:"status,omitempty"`
	Err          string    `json:"err"`
}

// BoardWriter has the same guarantees as Writer, for BoardEvent lines.
type BoardWriter struct{ lw lineWriter }

// NewBoardWriter wraps w.
func NewBoardWriter(w io.Writer) *BoardWriter { return &BoardWriter{lw: lineWriter{w: w}} }

// Err returns the first write/flush error, if any.
func (w *BoardWriter) Err() error { return w.lw.Err() }

// Write appends e. A failure (including a short write) is returned and kept.
func (w *BoardWriter) Write(e BoardEvent) error { return w.lw.write(e) }

// lineWriter is the shared single-write, flush, sticky-error JSONL logic.
type lineWriter struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

func (l *lineWriter) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *lineWriter) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return l.fail(err)
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.w.Write(b)
	if err == nil && n < len(b) {
		err = io.ErrShortWrite
	}
	if err == nil {
		switch f := l.w.(type) {
		case interface{ Flush() error }:
			err = f.Flush()
		case interface{ Sync() error }:
			err = f.Sync()
		}
	}
	if err != nil && l.err == nil {
		l.err = err
	}
	return err
}

func (l *lineWriter) fail(err error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err == nil {
		l.err = err
	}
	return err
}
