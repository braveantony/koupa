// Package board serves the call-number board's SSE stream (plan A3, A4).
//
// Each connection owns one buffered channel registered with the Pod's
// broadcaster and is written only by its own handler goroutine. The
// channel is registered before anything is read from the database, so it
// is both the holding area during the backfill and the live feed after
// it: switching from one to the other needs no hand-over and leaves no
// gap. Duplicates (a call both backfilled and buffered) are dropped by
// comparing against the last ready_seq sent.
package board

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"koupa/app/applog"
	"koupa/app/broadcast"
	"koupa/app/config"
	"koupa/app/hooks"
	"koupa/app/store"
	"koupa/internal/wire"
)

// ChannelBuffer is each connection's channel size. The broadcaster never
// blocks on a full channel: it marks the subscriber overflowed and the
// connection ends (the board reconnects). 64 calls is far more than a
// healthy board falls behind by, and small enough that a stuck one is cut
// off quickly instead of holding memory.
const ChannelBuffer = 64

// backfillPage is how many rows one backfill query reads.
const backfillPage = 500

// HookAfterHello is the pause point between A3 steps 4 and 5.
const HookAfterHello = "sse.after_hello"

// Registrar is the part of *broadcast.Broadcaster the handler uses.
type Registrar interface {
	Register(ctx context.Context, ch chan<- broadcast.ReadyEvent) (subID uint64, progress int64, err error)
	Unregister(subID uint64)
	Overflowed(subID uint64) <-chan struct{}
}

// Handler serves GET /events.
type Handler struct {
	Broadcaster Registrar
	Pool        *pgxpool.Pool
	Cfg         config.Config
	Log         *slog.Logger
	// Shutdown is closed when the server begins shutting down; every
	// connection then sends retry: and a shutdown event and ends. nil
	// never fires.
	Shutdown <-chan struct{}
	// Hooks is the test pause point; nil means hooks.Noop.
	Hooks hooks.Point

	bufSize int // tests only; 0 means ChannelBuffer
}

var connSeq atomic.Uint64

// Close reasons, logged with EvBoardClosed.
const (
	reasonClientGone = "client_gone"
	reasonOverflow   = "overflow"
	reasonShutdown   = "shutdown"
	reasonWriteError = "write_error"
	reasonDBError    = "db_error"
)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	attempt := r.Header.Get(wire.HeaderAttempt)
	log := h.Log
	if log == nil {
		log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	log = log.With("conn", connSeq.Add(1), "attempt", attempt)
	hk := h.Hooks
	if hk == nil {
		hk = hooks.Noop{}
	}
	size := h.bufSize
	if size <= 0 {
		size = ChannelBuffer
	}

	// 1. Register first: from here on every call after progress is
	// buffered in ch.
	ch := make(chan broadcast.ReadyEvent, size)
	subID, progress, err := h.Broadcaster.Register(ctx, ch)
	if err != nil {
		return // client left before the broadcaster was ready
	}
	defer h.Broadcaster.Unregister(subID)
	overflow := h.Broadcaster.Overflowed(subID)

	// 2. Start point: the current max (new statement), or a validated
	// Last-Event-ID when resuming is on.
	cur, err := h.maxReadySeq()
	if err != nil {
		log.Warn(applog.EvBoardClosed, "reason", reasonDBError, "err", err.Error())
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	start := cur
	if raw := r.Header.Get("Last-Event-ID"); h.Cfg.ResumeFromLastID && raw != "" {
		n, ok := parseLastEventID(raw)
		if !ok || n > cur {
			log.Warn(applog.EvBadLastEventID, "value", raw, "max", cur)
			http.Error(w, "Last-Event-ID must be an integer between 0 and "+strconv.FormatInt(cur, 10), http.StatusBadRequest)
			return
		}
		start = n
	}

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sw := &streamWriter{w: w, rc: http.NewResponseController(w), deadline: h.Cfg.WriteDeadline, log: log, trace: h.Cfg.TestHooks}
	// Give the final chunk written after we return a fresh deadline.
	defer sw.refreshDeadline()

	// 3. and 4.
	sent := start
	closed := func(reason string, err error) {
		args := []any{"reason", reason, "sent", sent}
		if err != nil {
			args = append(args, "err", err.Error())
		}
		log.Info(applog.EvBoardClosed, args...)
	}
	if err := sw.write(encodeHello(start, h.Cfg.Version)); err != nil {
		closed(reasonWriteError, err)
		return
	}

	hk.Pause(ctx, HookAfterHello, attempt)

	// stop reports, without blocking, why the connection must end now
	// (and has already sent shutdown if that is the reason).
	stop := func() bool {
		select {
		case <-ctx.Done():
			closed(reasonClientGone, nil)
		case <-overflow:
			closed(reasonOverflow, nil)
		case <-h.Shutdown:
			h.sendShutdown(sw, closed)
		default:
			return false
		}
		return true
	}
	if stop() {
		return
	}

	// 5. Backfill start < ready_seq <= H, H from a new statement.
	hi, err := h.maxReadySeq()
	if err != nil {
		closed(reasonDBError, err)
		return
	}
	if progress > hi {
		// A1's commit order makes this impossible; continue anyway, the
		// channel filter below still neither drops nor repeats.
		log.Warn(applog.EvProgressAheadOfH, "progress", progress, "h", hi)
	}
	for sent < hi {
		evs, err := h.readyAfter(sent)
		if err != nil {
			closed(reasonDBError, err)
			return
		}
		for _, ev := range evs {
			if ev.ReadySeq > hi {
				break
			}
			if stop() {
				return
			}
			if err := sw.write(encodeReady(ev)); err != nil {
				closed(reasonWriteError, err)
				return
			}
			sent = ev.ReadySeq
		}
		if len(evs) < backfillPage {
			break
		}
	}

	// 6. Live: skip what the backfill already sent.
	var hb <-chan time.Time
	if h.Cfg.SSEHeartbeat > 0 {
		t := time.NewTicker(h.Cfg.SSEHeartbeat)
		defer t.Stop()
		hb = t.C
	}
	for {
		select {
		case <-ctx.Done():
			closed(reasonClientGone, nil)
			return
		case <-overflow:
			closed(reasonOverflow, nil)
			return
		case <-h.Shutdown:
			h.sendShutdown(sw, closed)
			return
		case ev := <-ch:
			if ev.ReadySeq <= sent {
				continue
			}
			if stop() {
				return
			}
			if err := sw.write(encodeReady(ev)); err != nil {
				closed(reasonWriteError, err)
				return
			}
			sent = ev.ReadySeq
		case <-hb:
			if err := sw.write(heartbeat); err != nil {
				closed(reasonWriteError, err)
				return
			}
		}
	}
}

// sendShutdown writes retry: and the shutdown event (7.).
func (h *Handler) sendShutdown(sw *streamWriter, closed func(string, error)) {
	if err := sw.write(encodeShutdown(h.Cfg.SSERetry)); err != nil {
		closed(reasonWriteError, err)
		return
	}
	closed(reasonShutdown, nil)
}

func (h *Handler) maxReadySeq() (int64, error) {
	dctx, cancel := store.DBContext()
	defer cancel()
	return store.MaxReadySeq(dctx, h.Pool)
}

func (h *Handler) readyAfter(after int64) ([]broadcast.ReadyEvent, error) {
	dctx, cancel := store.DBContext()
	defer cancel()
	return store.ReadyAfter(dctx, h.Pool, after, backfillPage)
}

// parseLastEventID accepts only plain base-10 digits (no sign, spaces or
// decimal point) that fit in int64.
func parseLastEventID(s string) (int64, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// streamWriter writes one block, flushes it, and bounds the write with a
// deadline so a stuck board cannot hold the handler (A5).
type streamWriter struct {
	w        http.ResponseWriter
	rc       *http.ResponseController
	deadline time.Duration
	log      *slog.Logger
	trace    bool // log write start/end (TEST_HOOKS only)
	n        uint64
}

func (s *streamWriter) refreshDeadline() {
	if s.deadline > 0 {
		_ = s.rc.SetWriteDeadline(time.Now().Add(s.deadline))
	}
}

func (s *streamWriter) write(b []byte) error {
	s.n++
	if s.deadline > 0 {
		if err := s.rc.SetWriteDeadline(time.Now().Add(s.deadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
	}
	if s.trace {
		s.log.Info(applog.EvWriteStart, "write", s.n, "bytes", len(b))
	}
	_, err := s.w.Write(b)
	if err == nil {
		err = s.rc.Flush()
	}
	if s.trace {
		args := []any{"write", s.n}
		if err != nil {
			args = append(args, "err", err.Error())
		}
		s.log.Info(applog.EvWriteEnd, args...)
	}
	return err
}
