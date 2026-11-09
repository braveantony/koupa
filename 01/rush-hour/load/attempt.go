package load

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"

	"koupa/internal/wire"
	"koupa/rush-hour/record"
)

// call describes one HTTP request attempt: how to send it and which
// per-attempt timeout applies. It is shared by Customer, Kitchen and Prober
// so the send/classify/record logic exists once.
type call struct {
	Client  *http.Client
	Timeout time.Duration
	Method  string
	URL     string
	Body    []byte // nil for no body; a non-nil body is sent as JSON
}

// send performs c as the attempt described by rec (identity fields Run, Kind,
// ClientRef, Attempt, Try, ScheduledAt already set) and returns rec filled in:
// timestamps, scheduling lag, status, pod_uid, error kind and full_response.
// onOK, when non-nil, receives the record and the body of a complete 2xx
// response so the caller can pull out its own fields (order_id, ready_seq).
func send(ctx context.Context, c call, rec record.Attempt, onOK func(rec *record.Attempt, body []byte)) record.Attempt {
	actx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	var body io.Reader
	if c.Body != nil {
		body = bytes.NewReader(c.Body)
	}
	req, err := http.NewRequestWithContext(actx, c.Method, c.URL, body)
	if err != nil {
		rec.SentAt = time.Now()
		rec.RespondedAt = rec.SentAt
		return rec
	}
	if c.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(wire.HeaderAttempt, rec.Attempt)

	rec.SentAt = time.Now()
	rec.SchedLagMs = LagMs(rec.ScheduledAt, rec.SentAt)
	resp, err := c.Client.Do(req)
	if err != nil {
		rec.RespondedAt = time.Now()
		rec.ErrKind = classify(err)
		return rec
	}
	defer resp.Body.Close()
	st := resp.StatusCode
	rec.Status = &st
	rec.PodUID = resp.Header.Get(wire.HeaderPodUID)
	data, err := io.ReadAll(resp.Body)
	rec.RespondedAt = time.Now()
	if err != nil {
		rec.ErrKind = classify(err)
		return rec
	}
	rec.FullResponse = true
	if onOK != nil && st/100 == 2 {
		onOK(&rec, data)
	}
	return rec
}

// sendWithRetry runs do(try) for try = 1..1+retries, writing every attempt.
// It stops early when the attempt succeeded (a complete 2xx response) or
// more() reports false. A record-write error is returned (and stays in
// w.Err()); sending continues so the measurement is not silently shortened.
func sendWithRetry(w *record.Writer, retries int, more func() bool, do func(try int) record.Attempt) error {
	var werr error
	for try := 1; try <= 1+retries; try++ {
		rec := do(try)
		if err := w.Write(rec); err != nil && werr == nil {
			werr = err
		}
		if !shouldRetry(rec) || !more() {
			break
		}
	}
	return werr
}

func shouldRetry(r record.Attempt) bool {
	return !r.FullResponse || r.Status == nil || *r.Status/100 != 2
}

func classify(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return record.ErrTimeout
	case errors.Is(err, syscall.ECONNREFUSED):
		return record.ErrRefused
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return record.ErrReset
	}
	return ""
}
