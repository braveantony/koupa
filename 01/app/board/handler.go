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

// readTimeout caps each read query (the global 60s DB limit).
const readTimeout = 60 * time.Second

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

	bufSize   int    // tests only; 0 means ChannelBuffer
	heartbeat []byte // tests only; nil means ": hb\n" (a large filler fills socket buffers)
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

	// Reads are cancelled as soon as the connection must end (client
	// gone, overflow, shutdown), so a stalled query cannot hold the
	// handler past SHUTDOWN_TIMEOUT; each read is still capped at 60s.
	qctx, qcancel := context.WithCancel(ctx)
	defer qcancel()
	go func() {
		select {
		case <-overflow:
		case <-h.Shutdown:
		case <-qctx.Done():
		}
		qcancel()
	}()

	var sent int64
	closed := func(reason string, err error) {
		args := []any{"reason", reason, "sent", sent}
		if err != nil {
			args = append(args, "err", err.Error())
		}
		log.Info(applog.EvBoardClosed, args...)
	}
	sw := &streamWriter{w: w, rc: http.NewResponseController(w), deadline: h.Cfg.WriteDeadline, log: log, trace: h.Cfg.TestHooks}
	startStream := func() {
		hdr := w.Header()
		hdr.Set("Content-Type", "text/event-stream")
		hdr.Set("Cache-Control", "no-cache")
		hdr.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}

	// 2. Start point: the current max (new statement), or a validated
	// Last-Event-ID when resuming is on.
	cur, err := h.maxReadySeq(qctx)
	if err != nil {
		select {
		case <-h.Shutdown:
			// Shutdown before hello: still tell the board to reconnect.
			startStream()
			defer sw.refreshDeadline()
			h.sendShutdown(sw, closed)
		case <-ctx.Done():
			closed(reasonClientGone, nil)
		case <-overflow:
			closed(reasonOverflow, nil)
		default:
			log.Warn(applog.EvBoardClosed, "reason", reasonDBError, "err", err.Error())
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	start := cur
	// Absent header means first connect; a present but empty one is
	// malformed (EventSource never sends an empty Last-Event-ID).
	if vals, present := r.Header[http.CanonicalHeaderKey("Last-Event-ID")]; h.Cfg.ResumeFromLastID && present {
		raw := vals[0]
		n, ok := parseLastEventID(raw)
		if !ok || n > cur {
			log.Warn(applog.EvBadLastEventID, "value", raw, "max", cur)
			http.Error(w, "Last-Event-ID must be an integer between 0 and "+strconv.FormatInt(cur, 10), http.StatusBadRequest)
			return
		}
		start = n
	}

	startStream()
	hbData := heartbeat
	if h.Cfg.TestHooks && h.Cfg.SSEFillerBytes > 0 {
		hbData = filler(h.Cfg.SSEFillerBytes)
	}
	if h.heartbeat != nil {
		hbData = h.heartbeat
	}
	// Bound the final chunk net/http writes after we return.
	defer sw.refreshDeadline()

	// 3. and 4.
	sent = start
	if err := sw.write(encodeHello(start, h.Cfg.Version, h.Cfg.PodName, h.Cfg.PodUID)); err != nil {
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
	hi, err := h.maxReadySeq(qctx)
	if err != nil {
		if !stop() {
			closed(reasonDBError, err)
		}
		return
	}
	if progress > hi {
		// A1's commit order makes this impossible; continue anyway, the
		// channel filter below still neither drops nor repeats.
		log.Warn(applog.EvProgressAheadOfH, "progress", progress, "h", hi)
	}
	for sent < hi {
		evs, err := h.readyAfter(qctx, sent)
		if err != nil {
			if !stop() {
				closed(reasonDBError, err)
			}
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
			if err := sw.write(hbData); err != nil {
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

func (h *Handler) maxReadySeq(ctx context.Context) (int64, error) {
	dctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	return store.MaxReadySeq(dctx, h.Pool)
}

func (h *Handler) readyAfter(ctx context.Context, after int64) ([]broadcast.ReadyEvent, error) {
	dctx, cancel := context.WithTimeout(ctx, readTimeout)
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

// blockedAfter is how long a traced write may take before the monitor logs
// it as blocked (plan A12 timing 5).
var blockedAfter = time.Second

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
		// Unsupported (http.ErrNotSupported) is an error too: without a
		// deadline a stuck board could hold the handler forever.
		if err := s.rc.SetWriteDeadline(time.Now().Add(s.deadline)); err != nil {
			return err
		}
	}
	stopMon := func() bool { return false }
	if s.trace {
		s.log.Info(applog.EvWriteStart, "write", s.n, "bytes", len(b))
		// The monitor: a timer goroutine independent of the (possibly
		// blocked) write, so the record appears while the write is stuck.
		n, started := s.n, time.Now()
		mon := time.AfterFunc(blockedAfter, func() {
			s.log.Warn(applog.EvWriteBlocked, "write", n, "bytes", len(b),
				"started_at", started.UTC().Format(time.RFC3339Nano), "after_ms", blockedAfter.Milliseconds())
		})
		stopMon = mon.Stop
	}
	_, err := s.w.Write(b)
	if err == nil {
		err = s.rc.Flush()
	}
	stopMon()
	if err == nil && s.deadline > 0 {
		// Clear it: an idle healthy board must not fail its next write
		// because a stale deadline expired in between.
		err = s.rc.SetWriteDeadline(time.Time{})
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
