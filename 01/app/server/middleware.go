package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"koupa/app/applog"
	"koupa/app/store"
	"koupa/internal/wire"
)

// txRecorder carries a handler's transaction outcome to the middleware.
type txRecorder struct {
	mu  sync.Mutex
	res store.TxResult
	set bool
}

type recorderKey struct{}

// recordTx stores the request's write-transaction outcome for the
// request_end log. The last call wins.
func recordTx(r *http.Request, res store.TxResult) {
	if rec, ok := r.Context().Value(recorderKey{}).(*txRecorder); ok {
		rec.mu.Lock()
		rec.res, rec.set = res, true
		rec.mu.Unlock()
	}
}

// respWriter records the status and the first write and flush errors. It
// keeps Flush working and exposes the underlying writer through Unwrap so
// http.NewResponseController can still set write deadlines.
type respWriter struct {
	http.ResponseWriter
	status   int
	writeErr error
	flushErr error
}

func (w *respWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *respWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	if err != nil && w.writeErr == nil {
		w.writeErr = err
	}
	return n, err
}

// FlushError is what http.ResponseController.Flush calls.
func (w *respWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err != nil && w.flushErr == nil {
		w.flushErr = err
	}
	return err
}

func (w *respWriter) Flush() { _ = w.FlushError() }

func (w *respWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// wrap adds X-Koupa-Pod-UID to every response and logs request_received
// and request_end (plan A10 「紀錄」) for every request.
func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		s.inflight.Add(1)
		defer s.inflight.Add(-1)
		attempt := r.Header.Get(wire.HeaderAttempt)
		path := r.URL.Path
		s.log.Info(applog.EvRequestReceived, "attempt", attempt, "method", r.Method, "path", path)

		w.Header().Set(wire.HeaderPodUID, s.cfg.PodUID)
		rec := &txRecorder{}
		r = r.WithContext(context.WithValue(r.Context(), recorderKey{}, rec))
		rw := &respWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)

		status := rw.status
		if status == 0 {
			status = http.StatusOK // net/http sends 200 for a handler that wrote nothing
		}
		rec.mu.Lock()
		txr, hasTx := rec.res, rec.set
		rec.mu.Unlock()
		outcome, commitAt := "", ""
		if hasTx {
			outcome = string(txr.Outcome)
			if !txr.CommitSentAt.IsZero() {
				commitAt = txr.CommitSentAt.UTC().Format(time.RFC3339Nano)
			}
		}
		if txr.Outcome == store.TxCommitUnknown {
			s.log.Warn(applog.EvCommitUnknown, "attempt", attempt, "path", path, "commit_sent_at", commitAt)
		}
		s.log.Info(applog.EvRequestEnd,
			"attempt", attempt,
			"method", r.Method,
			"path", path,
			"status", status,
			"write_err", errString(rw.writeErr),
			"flush_err", errString(rw.flushErr),
			"ctx_canceled", r.Context().Err() != nil,
			"tx_outcome", outcome,
			"commit_sent_at", commitAt,
			"duration_ms", float64(time.Since(start).Microseconds())/1000,
		)
	})
}
