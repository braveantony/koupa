package board

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"koupa/app/applog"
	"koupa/app/broadcast"
	"koupa/app/config"
	"koupa/app/hooks"
	"koupa/app/store"
	"koupa/internal/testdb"
	"koupa/internal/wire"
)

const waitTimeout = 5 * time.Second

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// env is one test database, a running broadcaster and a probe subscriber.
// The probe receives every dispatch; because the broadcaster offers an
// event to all subscribers under one lock, once the probe has seq every
// board's channel has been offered it too.
type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	b     *broadcast.Broadcaster
	probe chan broadcast.ReadyEvent
	logs  *syncBuf
	log   *slog.Logger
}

func newEnv(t *testing.T, poll time.Duration) *env {
	t.Helper()
	pool := testdb.New(t)
	cc := pool.Config().ConnConfig
	u, err := url.Parse(os.Getenv("KOUPA_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(cc.User, cc.Password)
	u.Path = "/" + cc.Database
	logs := &syncBuf{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	b := broadcast.New(pool, u.String(), poll, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(waitTimeout):
			t.Error("broadcaster did not stop")
		}
	})
	select {
	case <-b.Ready():
	case <-time.After(waitTimeout):
		t.Fatal("broadcaster not ready")
	}
	e := &env{t: t, pool: pool, b: b, probe: make(chan broadcast.ReadyEvent, 1024), logs: logs, log: log}
	rctx, rcancel := context.WithTimeout(context.Background(), waitTimeout)
	defer rcancel()
	id, _, err := b.Register(rctx, e.probe)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Unregister(id) })
	return e
}

func baseCfg(resume bool) config.Config {
	return config.Config{
		Version:          "v2",
		ResumeFromLastID: resume,
		SSERetry:         1500 * time.Millisecond,
		WriteDeadline:    2 * time.Second,
	}
}

// serve starts an httptest server for h (filling defaults from e).
func (e *env) serve(h *Handler) string {
	e.t.Helper()
	if h.Broadcaster == nil {
		h.Broadcaster = e.b
	}
	if h.Pool == nil {
		h.Pool = e.pool
	}
	if h.Log == nil {
		h.Log = e.log
	}
	srv := httptest.NewServer(h)
	e.t.Cleanup(srv.Close)
	return srv.URL
}

func (e *env) createOrder() int64 {
	e.t.Helper()
	dctx, cancel := store.DBContext()
	defer cancel()
	var o store.Order
	if _, err := store.RunWriteTx(dctx, e.pool, store.TxOptions{}, func(c context.Context, tx pgx.Tx) error {
		var err error
		o, err = store.CreateOrderTx(c, tx, store.OrderRequest{Items: []store.OrderItem{{MenuItemID: 1, Quantity: 1}}}, false)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return o.ID
}

// mark creates an order and marks it ready (with NOTIFY); returns ready_seq.
func (e *env) mark() int64 {
	e.t.Helper()
	id := e.createOrder()
	dctx, cancel := store.DBContext()
	defer cancel()
	r, _, err := store.MarkReady(dctx, e.pool, id, "t", 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return r.ReadySeq
}

// markSilent marks an order ready WITHOUT notifying, so the broadcaster
// only learns of it on its next trigger.
func (e *env) markSilent() int64 {
	e.t.Helper()
	id := e.createOrder()
	dctx, cancel := store.DBContext()
	defer cancel()
	var seq int64
	if err := e.pool.QueryRow(dctx,
		`UPDATE orders SET status = 'ready', ready_seq = nextval('ready_seq_seq'),
		        ready_at = clock_timestamp(), ready_attempt = 't'
		 WHERE id = $1 RETURNING ready_seq`, id).Scan(&seq); err != nil {
		e.t.Fatal(err)
	}
	return seq
}

// awaitProbe waits until the broadcaster has dispatched seq.
func (e *env) awaitProbe(seq int64) {
	e.t.Helper()
	deadline := time.After(waitTimeout)
	for {
		select {
		case ev := <-e.probe:
			if ev.ReadySeq >= seq {
				return
			}
		case <-deadline:
			e.t.Fatalf("broadcaster did not dispatch %d", seq)
		}
	}
}

// --- minimal raw SSE client -------------------------------------------

type sseConn struct {
	t      *testing.T
	resp   *http.Response
	lines  chan string // closed at EOF
	cancel context.CancelFunc
}

// dial opens GET url with optional headers. On non-200 it returns the
// status and a nil conn.
func dial(t *testing.T, u string, hdr map[string]string) (*sseConn, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		cancel()
		return nil, resp.StatusCode
	}
	c := &sseConn{t: t, resp: resp, lines: make(chan string, 4096), cancel: cancel}
	go func() {
		defer close(c.lines)
		r := bufio.NewReader(resp.Body)
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				return
			}
			select {
			case c.lines <- strings.TrimSuffix(l, "\n"):
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(c.close)
	return c, http.StatusOK
}

func mustDial(t *testing.T, u string, hdr map[string]string) *sseConn {
	t.Helper()
	c, st := dial(t, u, hdr)
	if c == nil {
		t.Fatalf("status %d, want 200", st)
	}
	return c
}

func (c *sseConn) close() { c.cancel(); c.resp.Body.Close() }

// line returns the next raw line; ok is false at EOF.
func (c *sseConn) line() (string, bool) {
	c.t.Helper()
	select {
	case l, ok := <-c.lines:
		return l, ok
	case <-time.After(waitTimeout):
		c.t.Fatal("no line received in time")
		return "", false
	}
}

type sseEvent struct {
	event, id, data, retry string
	hasData                bool
}

// next parses the next dispatched block, skipping comment lines.
func (c *sseConn) next() sseEvent {
	c.t.Helper()
	var ev sseEvent
	started := false
	for {
		l, ok := c.line()
		if !ok {
			c.t.Fatalf("EOF while waiting for an event (partial %+v)", ev)
		}
		if strings.HasPrefix(l, ":") {
			continue
		}
		if l == "" {
			if started {
				return ev
			}
			continue
		}
		started = true
		k, v, _ := strings.Cut(l, ":")
		v = strings.TrimPrefix(v, " ")
		switch k {
		case "event":
			ev.event = v
		case "id":
			ev.id = v
		case "data":
			ev.data, ev.hasData = v, true
		case "retry":
			ev.retry = v
		}
	}
}

func (c *sseConn) hello() int64 {
	c.t.Helper()
	ev := c.next()
	if ev.event != "hello" {
		c.t.Fatalf("first event %+v, want hello", ev)
	}
	n, err := strconv.ParseInt(ev.id, 10, 64)
	if err != nil {
		c.t.Fatal(err)
	}
	return n
}

// readyUntil collects ready ids until (and including) sentinel.
func (c *sseConn) readyUntil(sentinel int64) []int64 {
	c.t.Helper()
	var got []int64
	for {
		ev := c.next()
		if ev.event != "ready" {
			c.t.Fatalf("got %+v, want ready", ev)
		}
		n, err := strconv.ParseInt(ev.id, 10, 64)
		if err != nil {
			c.t.Fatal(err)
		}
		got = append(got, n)
		if n >= sentinel {
			if n > sentinel {
				c.t.Fatalf("passed sentinel %d: %v", sentinel, got)
			}
			return got
		}
	}
}

// expectEOF reads until the server ends the stream.
func (c *sseConn) expectEOF() {
	c.t.Helper()
	for {
		if _, ok := c.line(); !ok {
			return
		}
	}
}

func seqs(from, to int64) []int64 {
	var s []int64
	for i := from; i <= to; i++ {
		s = append(s, i)
	}
	return s
}

func lastID(n int64) map[string]string {
	return map[string]string{"Last-Event-ID": strconv.FormatInt(n, 10)}
}

// recordingBroadcaster wraps the real one; Register reports progress+offset
// and records the real progress and subscriber ids; Unregister reports
// each id on unreg.
type recordingBroadcaster struct {
	*broadcast.Broadcaster
	offset int64
	mu     sync.Mutex
	got    []int64
	ids    []uint64
	unreg  chan uint64 // buffered; created by newRecording
}

func newRecording(b *broadcast.Broadcaster) *recordingBroadcaster {
	return &recordingBroadcaster{Broadcaster: b, unreg: make(chan uint64, 64)}
}

func (r *recordingBroadcaster) Register(ctx context.Context, ch chan<- broadcast.ReadyEvent) (uint64, int64, error) {
	id, p, err := r.Broadcaster.Register(ctx, ch)
	r.mu.Lock()
	r.got = append(r.got, p)
	r.ids = append(r.ids, id)
	r.mu.Unlock()
	return id, p + r.offset, err
}

func (r *recordingBroadcaster) Unregister(id uint64) {
	r.Broadcaster.Unregister(id)
	if r.unreg != nil {
		r.unreg <- id
	}
}

func (r *recordingBroadcaster) subIDs() []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ids)
}

// awaitUnregister waits until id is unregistered.
func (r *recordingBroadcaster) awaitUnregister(t *testing.T, id uint64, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case got := <-r.unreg:
			if got == id {
				return
			}
		case <-deadline:
			t.Fatalf("subscriber %d not unregistered within %v", id, within)
		}
	}
}

func (r *recordingBroadcaster) progress() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got)
}

// funcHooks runs fn at every pause point.
type funcHooks func(ctx context.Context, name, attemptID string)

func (f funcHooks) Pause(ctx context.Context, name, attemptID string) { f(ctx, name, attemptID) }

var _ hooks.Point = funcHooks(nil)

func maxSeq(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	dctx, cancel := store.DBContext()
	defer cancel()
	m, err := store.MaxReadySeq(dctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// --- A3 must-tests ----------------------------------------------------

// Progress at Register (3) is behind H (5): 4 and 5 arrive both by
// backfill and later through the channel; the channel copies are skipped.
func TestProgressBehindH(t *testing.T) {
	e := newEnv(t, time.Hour) // NOTIFY only; no poll catch-up
	for range 3 {
		e.mark()
	}
	e.awaitProbe(3)
	e.markSilent() // 4
	e.markSilent() // 5
	rb := &recordingBroadcaster{Broadcaster: e.b}
	u := e.serve(&Handler{Broadcaster: rb, Cfg: baseCfg(true)})

	c := mustDial(t, u, lastID(2))
	if got := c.hello(); got != 2 {
		t.Fatalf("hello id %d, want 2", got)
	}
	if got := c.readyUntil(5); !slices.Equal(got, []int64{3, 4, 5}) {
		t.Fatalf("backfill %v", got)
	}
	if p := rb.progress(); len(p) != 1 || p[0] != 3 {
		t.Fatalf("progress at register %v, want [3] (behind H=5)", p)
	}
	s := e.mark() // 6: broadcaster dispatches 4, 5, 6
	if got := c.readyUntil(s); !slices.Equal(got, []int64{6}) {
		t.Fatalf("live %v, want [6]", got)
	}
}

func TestProgressEqualsH(t *testing.T) {
	e := newEnv(t, 200*time.Millisecond)
	for range 3 {
		e.mark()
	}
	e.awaitProbe(3)
	rb := &recordingBroadcaster{Broadcaster: e.b}
	u := e.serve(&Handler{Broadcaster: rb, Cfg: baseCfg(true)})

	c := mustDial(t, u, lastID(1))
	if got := c.hello(); got != 1 {
		t.Fatalf("hello id %d", got)
	}
	if got := c.readyUntil(3); !slices.Equal(got, []int64{2, 3}) {
		t.Fatalf("backfill %v", got)
	}
	if p := rb.progress(); len(p) != 1 || p[0] != 3 {
		t.Fatalf("progress at register %v, want [3] (= H)", p)
	}
	e.mark()
	s := e.mark()
	if got := c.readyUntil(s); !slices.Equal(got, []int64{4, 5}) {
		t.Fatalf("live %v", got)
	}
}

// fakeRegistrar hands the board's channel to the test and reports a fixed
// progress, so the test controls exactly what arrives on the channel.
type fakeRegistrar struct {
	progress int64
	chs      chan chan<- broadcast.ReadyEvent
	unreg    chan uint64
}

func (f *fakeRegistrar) Register(ctx context.Context, ch chan<- broadcast.ReadyEvent) (uint64, int64, error) {
	f.chs <- ch
	return 1, f.progress, nil
}
func (f *fakeRegistrar) Unregister(id uint64)              { f.unreg <- id }
func (f *fakeRegistrar) Overflowed(uint64) <-chan struct{} { return nil }

// A fake Register reports progress 5 while H is 2 (the anomaly A1 rules
// out). The channel then carries already-sent calls (1, 2, a repeated 3,
// 4) mixed with new ones; the board must get 1..5 exactly once each, and
// the handler must warn.
func TestProgressAheadOfHWithFake(t *testing.T) {
	e := newEnv(t, 200*time.Millisecond)
	e.mark()
	e.mark()
	fake := &fakeRegistrar{progress: 5, chs: make(chan chan<- broadcast.ReadyEvent, 1), unreg: make(chan uint64, 1)}
	u := e.serve(&Handler{Broadcaster: fake, Cfg: baseCfg(true)})

	c := mustDial(t, u, lastID(0))
	var ch chan<- broadcast.ReadyEvent
	select {
	case ch = <-fake.chs:
	case <-time.After(waitTimeout):
		t.Fatal("no Register")
	}
	for _, s := range []int64{1, 2, 3, 2, 3, 4, 4, 5} {
		ch <- broadcast.ReadyEvent{ReadySeq: s, OrderID: 100 + s, Number: store.FormatNumber(s)}
	}
	if got := c.hello(); got != 0 {
		t.Fatalf("hello id %d", got)
	}
	if got := c.readyUntil(5); !slices.Equal(got, seqs(1, 5)) {
		t.Fatalf("got %v, want 1..5 once each", got)
	}
	if !strings.Contains(e.logs.String(), `"msg":"progress_ahead_of_h","conn":`) ||
		!strings.Contains(e.logs.String(), `"progress":5,"h":2`) {
		t.Fatalf("no progress_ahead_of_h warning with progress 5, h 2; logs:\n%s", e.logs.String())
	}
	c.close()
	select {
	case <-fake.unreg:
	case <-time.After(waitTimeout):
		t.Fatal("not unregistered")
	}
}

// A call committed between Register and the H query is in both the
// backfill and the channel; the board sees it exactly once.
func TestCommitBetweenRegisterAndH(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run("resume="+strconv.FormatBool(resume), func(t *testing.T) {
			e := newEnv(t, 200*time.Millisecond)
			e.mark()
			e.awaitProbe(1)
			var hookSeq int64
			var calls []string
			var mu sync.Mutex
			h := funcHooks(func(ctx context.Context, name, attempt string) {
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, name+"/"+attempt)
				hookSeq = e.mark()
				e.awaitProbe(hookSeq) // now in the board's channel too
			})
			u := e.serve(&Handler{Cfg: baseCfg(resume), Hooks: h})
			c := mustDial(t, u, map[string]string{wire.HeaderAttempt: "run1-a1-t1"})
			if got := c.hello(); got != 1 {
				t.Fatalf("hello id %d", got)
			}
			if got := c.readyUntil(2); !slices.Equal(got, []int64{2}) {
				t.Fatalf("handoff %v", got)
			}
			s := e.mark()
			if got := c.readyUntil(s); !slices.Equal(got, []int64{3}) {
				t.Fatalf("live %v, want [3] (no duplicate 2)", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(calls, []string{"sse.after_hello/run1-a1-t1"}) || hookSeq != 2 {
				t.Fatalf("hook calls %v seq %d", calls, hookSeq)
			}
		})
	}
}

func TestReconnectAfterHelloOnly(t *testing.T) {
	e := newEnv(t, 200*time.Millisecond)
	e.mark()
	u := e.serve(&Handler{Cfg: baseCfg(true)})
	c := mustDial(t, u, nil)
	first := c.hello()
	c.close()
	e.mark()
	e.mark()

	c2 := mustDial(t, u, lastID(first))
	if got := c2.hello(); got != first {
		t.Fatalf("hello id %d, want %d", got, first)
	}
	if got := c2.readyUntil(first + 2); !slices.Equal(got, seqs(first+1, first+2)) {
		t.Fatalf("backfill %v", got)
	}
	s := e.mark()
	if got := c2.readyUntil(s); !slices.Equal(got, []int64{s}) {
		t.Fatalf("live %v", got)
	}
}

func TestEmptyDatabaseStartsAtZero(t *testing.T) {
	e := newEnv(t, 200*time.Millisecond)
	u := e.serve(&Handler{Cfg: baseCfg(true)})
	c := mustDial(t, u, nil)
	if got := c.hello(); got != 0 {
		t.Fatalf("hello id %d, want 0", got)
	}
	c2 := mustDial(t, u, lastID(0)) // 0 is valid on an empty database
	if got := c2.hello(); got != 0 {
		t.Fatalf("resume hello id %d, want 0", got)
	}
	s := e.mark()
	if got := c.readyUntil(s); !slices.Equal(got, []int64{1}) {
		t.Fatalf("got %v", got)
	}
	if got := c2.readyUntil(s); !slices.Equal(got, []int64{1}) {
		t.Fatalf("resumed got %v", got)
	}
}

func TestBadLastEventID400(t *testing.T) {
	e := newEnv(t, 200*time.Millisecond)
	e.mark()
	e.mark()
	u := e.serve(&Handler{Cfg: baseCfg(true)})
	for _, v := range []string{"abc", "-1", "3", "+1", "1.0"} {
		_, st := dial(t, u, map[string]string{"Last-Event-ID": v})
		if st != http.StatusBadRequest {
			t.Errorf("Last-Event-ID %q: status %d, want 400", v, st)
		}
	}
	if n := strings.Count(e.logs.String(), `"msg":"bad_last_event_id"`); n != 5 {
		t.Fatalf("%d bad_last_event_id lines, want 5; logs:\n%s", n, e.logs.String())
	}
	c := mustDial(t, u, lastID(2)) // the max itself is fine
	if got := c.hello(); got != 2 {
		t.Fatalf("hello %d", got)
	}
}

func TestResumeOffMissesGap(t *testing.T) {
	e := newEnv(t, 200*time.Millisecond)
	e.mark()
	u := e.serve(&Handler{Cfg: baseCfg(false)})
	c := mustDial(t, u, nil)
	first := c.hello()
	c.close()
	e.mark()
	e.mark()

	c2 := mustDial(t, u, lastID(first))
	if got := c2.hello(); got != first+2 {
		t.Fatalf("hello id %d, want current max %d", got, first+2)
	}
	s := e.mark()
	if got := c2.readyUntil(s); !slices.Equal(got, []int64{s}) {
		t.Fatalf("got %v, want only %d (gap missed)", got, s)
	}
}

// The board's channel (buffer 1) overflows while the handler is paused
// after hello; the connection ends without sending the overflowed calls.
func TestSlowBoardOverflow(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run("resume="+strconv.FormatBool(resume), func(t *testing.T) {
			e := newEnv(t, 200*time.Millisecond)
			e.mark()
			e.awaitProbe(1)
			h := funcHooks(func(ctx context.Context, name, attempt string) {
				if attempt != "slow" {
					return
				}
				for range 3 {
					e.mark()
				}
				e.awaitProbe(4)
			})
			u := e.serve(&Handler{Cfg: baseCfg(resume), Hooks: h, bufSize: 1})
			c := mustDial(t, u, map[string]string{wire.HeaderAttempt: "slow"})
			first := c.hello()
			if first != 1 {
				t.Fatalf("hello %d", first)
			}
			for {
				l, ok := c.line()
				if !ok {
					break
				}
				if strings.HasPrefix(l, "event: ready") {
					t.Fatalf("got a ready event after overflow")
				}
			}
			if !strings.Contains(e.logs.String(), `"reason":"overflow"`) {
				t.Fatalf("no overflow close logged:\n%s", e.logs.String())
			}

			c2 := mustDial(t, u, lastID(first))
			hello := c2.hello()
			s := e.mark()
			got := c2.readyUntil(s)
			if resume {
				if hello != 1 || !slices.Equal(got, seqs(2, 5)) {
					t.Fatalf("resume: hello %d got %v, want 1 and 2..5", hello, got)
				}
			} else {
				if hello != 4 || !slices.Equal(got, []int64{5}) {
					t.Fatalf("no resume: hello %d got %v, want 4 and [5]", hello, got)
				}
			}
		})
	}
}

// --- extra tests --------------------------------------------------------

func TestNoLastEventIDIsFirstConnect(t *testing.T) {
	e := newEnv(t, 200*time.Millisecond)
	e.mark()
	e.mark()
	u := e.serve(&Handler{Cfg: baseCfg(true)})
	c := mustDial(t, u, nil)
	if got := c.hello(); got != 2 {
		t.Fatalf("hello %d, want current max 2", got)
	}
	s := e.mark()
	if got := c.readyUntil(s); !slices.Equal(got, []int64{3}) {
		t.Fatalf("got %v", got)
	}
}

func TestEventWireFormat(t *testing.T) {
	e := newEnv(t, 200*time.Millisecond)
	cfg := baseCfg(true)
	cfg.SSEHeartbeat = 100 * time.Millisecond
	shutdown := make(chan struct{})
	u := e.serve(&Handler{Cfg: cfg, Shutdown: shutdown})
	c := mustDial(t, u, nil)
	if ct := c.resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q", ct)
	}
	expect := func(want ...string) {
		t.Helper()
		for _, w := range want {
			if l, ok := c.line(); !ok || l != w {
				t.Fatalf("line %q (ok=%v), want %q", l, ok, w)
			}
		}
	}
	// skipHB consumes heartbeat lines and returns the first other line.
	hbs := 0
	skipHB := func() string {
		t.Helper()
		for {
			l, ok := c.line()
			if !ok {
				t.Fatal("unexpected EOF")
			}
			if l == ": hb" {
				hbs++
				continue
			}
			return l
		}
	}
	expect("event: hello", "id: 0", `data: {"version":"v2"}`, "")
	seq := e.mark()
	var num int
	var oid int64
	dctx, cancel := store.DBContext()
	defer cancel()
	if err := e.pool.QueryRow(dctx, `SELECT number, id FROM orders WHERE ready_seq = $1`, seq).Scan(&num, &oid); err != nil {
		t.Fatal(err)
	}
	if l := skipHB(); l != "event: ready" {
		t.Fatalf("line %q, want event: ready", l)
	}
	expect("id: "+strconv.FormatInt(seq, 10),
		`data: {"number":"`+store.FormatNumber(int64(num))+`","order_id":`+strconv.FormatInt(oid, 10)+`}`, "")
	for hbs < 2 {
		if l, ok := c.line(); !ok || l != ": hb" {
			t.Fatalf("line %q (ok=%v), want heartbeat", l, ok)
		}
		hbs++
	}
	close(shutdown)
	if l := skipHB(); l != "retry: 1500" {
		t.Fatalf("line %q, want retry: 1500", l)
	}
	expect("event: shutdown", "data:", "")
	if l, ok := c.line(); ok {
		t.Fatalf("line %q after shutdown, want EOF", l)
	}
}

func TestHeartbeatInterval(t *testing.T) {
	e := newEnv(t, 200*time.Millisecond)
	cfg := baseCfg(true)
	cfg.SSEHeartbeat = 200 * time.Millisecond
	u := e.serve(&Handler{Cfg: cfg})
	c := mustDial(t, u, nil)
	c.hello()
	n := 0
	end := time.After(time.Second + 50*time.Millisecond)
	for {
		select {
		case l, ok := <-c.lines:
			if !ok {
				t.Fatal("EOF")
			}
			if strings.HasPrefix(l, ":") {
				n++
			}
			continue
		case <-end:
		}
		break
	}
	if n < 4 || n > 6 {
		t.Fatalf("%d comment lines in 1s at 200ms, want 4..6", n)
	}
}

// With heartbeat 0 no comment lines are sent.
func TestHeartbeatOff(t *testing.T) {
	e := newEnv(t, 200*time.Millisecond)
	u := e.serve(&Handler{Cfg: baseCfg(true)})
	c := mustDial(t, u, nil)
	c.hello()
	select {
	case l := <-c.lines:
		t.Fatalf("unexpected line %q with heartbeat off", l)
	case <-time.After(500 * time.Millisecond):
	}
}

// --- review fixes ---------------------------------------------------------

// An idle period longer than the write deadline must not break the next
// event or the shutdown: the deadline only bounds a write in progress.
func TestIdleLongerThanWriteDeadline(t *testing.T) {
	e := newEnv(t, 200*time.Millisecond)
	cfg := baseCfg(true)
	cfg.WriteDeadline = 200 * time.Millisecond
	shutdown := make(chan struct{})
	u := e.serve(&Handler{Cfg: cfg, Shutdown: shutdown})
	c := mustDial(t, u, nil)
	c.hello()
	time.Sleep(500 * time.Millisecond)
	s := e.mark()
	if got := c.readyUntil(s); !slices.Equal(got, []int64{s}) {
		t.Fatalf("got %v", got)
	}
	time.Sleep(500 * time.Millisecond)
	close(shutdown)
	if ev := c.next(); ev.event != "shutdown" || ev.retry != "1500" || !ev.hasData {
		t.Fatalf("got %+v, want shutdown", ev)
	}
	c.expectEOF()
	if strings.Contains(e.logs.String(), `"reason":"write_error"`) {
		t.Fatalf("write error logged:\n%s", e.logs.String())
	}
}

// A present but empty Last-Event-ID is malformed when resuming is on.
func TestEmptyLastEventID400(t *testing.T) {
	e := newEnv(t, 200*time.Millisecond)
	e.mark()
	on := e.serve(&Handler{Cfg: baseCfg(true)})
	if _, st := dial(t, on, map[string]string{"Last-Event-ID": ""}); st != http.StatusBadRequest {
		t.Fatalf("resume on, empty Last-Event-ID: status %d, want 400", st)
	}
	if !strings.Contains(e.logs.String(), `"msg":"bad_last_event_id"`) {
		t.Fatalf("no bad_last_event_id log:\n%s", e.logs.String())
	}
	off := e.serve(&Handler{Cfg: baseCfg(false)})
	c := mustDial(t, off, map[string]string{"Last-Event-ID": ""}) // ignored when off
	if got := c.hello(); got != 1 {
		t.Fatalf("hello %d", got)
	}
}

// While the H query is blocked on a table lock, shutdown and a client
// leaving both end the connection promptly and unregister it.
func TestBlockedQueryEnds(t *testing.T) {
	for _, mode := range []string{"shutdown", "client_gone"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t, time.Hour)
			e.mark()
			rb := newRecording(e.b)
			shutdown := make(chan struct{})
			h := funcHooks(func(ctx context.Context, name, attempt string) {
				tx, err := e.pool.Begin(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				t.Cleanup(func() { tx.Rollback(context.Background()) })
				if _, err := tx.Exec(context.Background(), `LOCK TABLE orders IN ACCESS EXCLUSIVE MODE`); err != nil {
					t.Error(err)
				}
			})
			u := e.serve(&Handler{Broadcaster: rb, Cfg: baseCfg(true), Shutdown: shutdown, Hooks: h})
			c := mustDial(t, u, nil)
			c.hello()
			// Wait until the H query is waiting on the lock.
			waitDeadline := time.Now().Add(waitTimeout)
			for {
				var n int
				if err := e.pool.QueryRow(context.Background(),
					`SELECT count(*) FROM pg_stat_activity
					 WHERE wait_event_type = 'Lock' AND query LIKE '%MAX(ready_seq)%' AND pid <> pg_backend_pid()`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n > 0 {
					break
				}
				if time.Now().After(waitDeadline) {
					t.Fatal("H query never blocked")
				}
				time.Sleep(20 * time.Millisecond)
			}
			id := rb.subIDs()[0]
			began := time.Now()
			if mode == "shutdown" {
				close(shutdown)
				if ev := c.next(); ev.event != "shutdown" {
					t.Fatalf("got %+v, want shutdown", ev)
				}
				c.expectEOF()
			} else {
				c.close()
			}
			rb.awaitUnregister(t, id, 2*time.Second)
			if d := time.Since(began); d > 2*time.Second {
				t.Fatalf("ended after %v", d)
			}
			if !strings.Contains(e.logs.String(), `"reason":"`+mode+`"`) {
				t.Fatalf("no %s close logged:\n%s", mode, e.logs.String())
			}
		})
	}
}

// smallBufListener shrinks each accepted connection's send buffer so a
// board that stops reading blocks the server's writes quickly.
type smallBufListener struct{ net.Listener }

func (l smallBufListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetWriteBuffer(4096)
	}
	return c, err
}

// rawBoard connects over plain TCP, reads up to the hello id, then stops
// reading for good.
func rawBoard(t *testing.T, addr string) (net.Conn, int64) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.(*net.TCPConn).SetReadBuffer(4096)
	conn.SetDeadline(time.Now().Add(waitTimeout))
	if _, err := io.WriteString(conn, "GET /events HTTP/1.1\r\nHost: board\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	seenHello := false
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("raw board: %v", err)
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "event: hello" {
			seenHello = true
		}
		if seenHello && strings.HasPrefix(l, "id: ") {
			n, err := strconv.ParseInt(strings.TrimPrefix(l, "id: "), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return conn, n
		}
	}
}

// A board that stops reading is cut off by the write deadline and
// unregistered; another board on the same server keeps receiving; on
// reconnect the stuck board resumes (on) or starts at the current max (off).
func TestBlockedWriterDeadline(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run("resume="+strconv.FormatBool(resume), func(t *testing.T) {
			e := newEnv(t, 200*time.Millisecond)
			e.mark()
			e.awaitProbe(1)
			rb := newRecording(e.b)
			cfg := baseCfg(resume)
			cfg.WriteDeadline = 300 * time.Millisecond
			cfg.SSEHeartbeat = 5 * time.Millisecond
			filler := []byte(": " + strings.Repeat("x", 64<<10) + "\n")
			srv := httptest.NewUnstartedServer(&Handler{Broadcaster: rb, Pool: e.pool, Log: e.log, Cfg: cfg, heartbeat: filler})
			srv.Listener = smallBufListener{srv.Listener}
			srv.Start()
			t.Cleanup(srv.Close)

			stuck, helloID := rawBoard(t, srv.Listener.Addr().String())
			healthy := mustDial(t, srv.URL, nil)
			healthy.hello()
			ids := rb.subIDs()
			if len(ids) != 2 {
				t.Fatalf("subscribers %v", ids)
			}
			began := time.Now()
			rb.awaitUnregister(t, ids[0], 3*time.Second)
			t.Logf("stuck board cut off after %v", time.Since(began))
			if !strings.Contains(e.logs.String(), `"reason":"write_error"`) {
				t.Fatalf("no write_error close:\n%s", e.logs.String())
			}
			// Calls while the stuck board is gone; the healthy one gets them.
			g1, g2 := e.mark(), e.mark()
			if got := healthy.readyUntil(g2); !slices.Equal(got, []int64{g1, g2}) {
				t.Fatalf("healthy board got %v", got)
			}
			stuck.Close()

			c := mustDial(t, srv.URL, lastID(helloID))
			hello := c.hello()
			s := e.mark()
			got := c.readyUntil(s)
			if resume {
				if hello != helloID || !slices.Equal(got, []int64{g1, g2, s}) {
					t.Fatalf("resume: hello %d got %v, want %d and [%d %d %d]", hello, got, helloID, g1, g2, s)
				}
			} else if hello != g2 || !slices.Equal(got, []int64{s}) {
				t.Fatalf("no resume: hello %d got %v, want %d and [%d]", hello, got, g2, s)
			}
			if got := healthy.readyUntil(s); !slices.Equal(got, []int64{s}) {
				t.Fatalf("healthy board got %v", got)
			}
		})
	}
}

// Shutdown while the FIRST (start point) query is blocked: the board still
// gets 200, retry: and the shutdown event, and the handler unregisters.
func TestShutdownDuringFirstQuery(t *testing.T) {
	e := newEnv(t, time.Hour)
	e.mark()
	rb := newRecording(e.b)
	shutdown := make(chan struct{})
	u := e.serve(&Handler{Broadcaster: rb, Cfg: baseCfg(true), Shutdown: shutdown})

	tx, err := e.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(context.Background()) })
	if _, err := tx.Exec(context.Background(), `LOCK TABLE orders IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	type result struct {
		resp *http.Response
		err  error
	}
	res := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*waitTimeout)
	defer cancel()
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		resp, err := http.DefaultClient.Do(req)
		res <- result{resp, err}
	}()
	waitDeadline := time.Now().Add(waitTimeout)
	for {
		var n int
		if err := e.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			 WHERE wait_event_type = 'Lock' AND query LIKE '%MAX(ready_seq)%' AND pid <> pg_backend_pid()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(waitDeadline) {
			t.Fatal("first query never blocked")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ids := rb.subIDs()
	if len(ids) != 1 {
		t.Fatalf("subscribers %v", ids)
	}
	close(shutdown)
	var r result
	select {
	case r = <-res:
	case <-time.After(2 * time.Second):
		t.Fatal("no response within 2s of shutdown")
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	defer r.resp.Body.Close()
	if r.resp.StatusCode != http.StatusOK || r.resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, Content-Type %q", r.resp.StatusCode, r.resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(r.resp.Body) // EOF means the handler returned
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "retry: 1500\nevent: shutdown\ndata:\n\n" {
		t.Fatalf("body %q", body)
	}
	rb.awaitUnregister(t, ids[0], 2*time.Second)
	if !strings.Contains(e.logs.String(), `"reason":"shutdown"`) {
		t.Fatalf("no shutdown close logged:\n%s", e.logs.String())
	}
}

// --- test-mode write monitor and filler (plan A12 timing 5) --------------

// With TEST_HOOKS a write that has not returned after blockedAfter is
// logged once as write_blocked (conn, write number, start time), while the
// write is still blocked: before its write_end.
func TestWriteBlockedLogged(t *testing.T) {
	e := newEnv(t, 200*time.Millisecond)
	cfg := baseCfg(true)
	cfg.TestHooks = true
	cfg.WriteDeadline = 3 * time.Second
	cfg.SSEHeartbeat = 5 * time.Millisecond
	filler := []byte(": " + strings.Repeat("x", 64<<10) + "\n")
	srv := httptest.NewUnstartedServer(&Handler{Broadcaster: e.b, Pool: e.pool, Log: e.log, Cfg: cfg, heartbeat: filler})
	srv.Listener = smallBufListener{srv.Listener}
	srv.Start()
	t.Cleanup(srv.Close)
	rawBoard(t, srv.Listener.Addr().String())

	deadline := time.Now().Add(waitTimeout)
	var blocked map[string]any
	for blocked == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		for _, l := range strings.Split(e.logs.String(), "\n") {
			m := map[string]any{}
			if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == applog.EvWriteBlocked {
				blocked = m
				break
			}
		}
	}
	if blocked == nil {
		t.Fatalf("no write_blocked line:\n%s", e.logs.String())
	}
	if blocked["conn"] == nil || blocked["write"] == nil || blocked["started_at"] == nil {
		t.Fatalf("write_blocked lacks conn/write/started_at: %v", blocked)
	}
	started, err := time.Parse(time.RFC3339Nano, blocked["started_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	at, _ := time.Parse(time.RFC3339Nano, blocked["time"].(string))
	if d := at.Sub(started); d < blockedAfter {
		t.Fatalf("write_blocked logged %v after start, want >= %v", d, blockedAfter)
	}
	// The blocked write's write_end comes after write_blocked.
	idxBlocked, idxEnd := -1, -1
	for i, l := range strings.Split(e.logs.String(), "\n") {
		m := map[string]any{}
		if json.Unmarshal([]byte(l), &m) != nil {
			continue
		}
		if m["msg"] == applog.EvWriteBlocked && idxBlocked < 0 {
			idxBlocked = i
		}
		if m["msg"] == applog.EvWriteEnd && m["write"] == blocked["write"] && m["conn"] == blocked["conn"] {
			idxEnd = i
		}
	}
	if idxEnd >= 0 && idxEnd < idxBlocked {
		t.Fatalf("write_end (line %d) before write_blocked (line %d)", idxEnd, idxBlocked)
	}
}

// Without TEST_HOOKS no write is traced or monitored.
func TestWriteBlockedNeedsTestHooks(t *testing.T) {
	e := newEnv(t, 200*time.Millisecond)
	cfg := baseCfg(true)
	cfg.WriteDeadline = 2 * time.Second
	cfg.SSEHeartbeat = 5 * time.Millisecond
	filler := []byte(": " + strings.Repeat("x", 64<<10) + "\n")
	srv := httptest.NewUnstartedServer(&Handler{Broadcaster: e.b, Pool: e.pool, Log: e.log, Cfg: cfg, heartbeat: filler})
	srv.Listener = smallBufListener{srv.Listener}
	srv.Start()
	t.Cleanup(srv.Close)
	rawBoard(t, srv.Listener.Addr().String())
	time.Sleep(blockedAfter + time.Second)
	if s := e.logs.String(); strings.Contains(s, applog.EvWriteBlocked) || strings.Contains(s, applog.EvWriteStart) {
		t.Fatalf("write tracing without TEST_HOOKS:\n%s", s)
	}
}

// With TEST_HOOKS and SSEFillerBytes the heartbeat is one comment line of
// exactly that many bytes; without TEST_HOOKS the setting is ignored.
func TestFillerSize(t *testing.T) {
	for _, hooks := range []bool{true, false} {
		t.Run("hooks="+strconv.FormatBool(hooks), func(t *testing.T) {
			e := newEnv(t, 200*time.Millisecond)
			cfg := baseCfg(true)
			cfg.TestHooks = hooks
			cfg.SSEHeartbeat = 20 * time.Millisecond
			cfg.SSEFillerBytes = 1000
			u := e.serve(&Handler{Cfg: cfg})
			c := mustDial(t, u, nil)
			c.hello()
			l, ok := c.line()
			if !ok {
				t.Fatal("EOF")
			}
			want := ": hb"
			if hooks {
				want = ": " + strings.Repeat("x", 1000-3)
			}
			if l != want {
				t.Fatalf("heartbeat line %q (len %d), want len %d", l[:min(len(l), 20)], len(l), len(want))
			}
		})
	}
}
