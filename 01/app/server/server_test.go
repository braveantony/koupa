package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"koupa/app/applog"
	"koupa/app/broadcast"
	"koupa/app/hooks"
	"koupa/internal/testdb"
)

type orderResp struct {
	OrderID int64  `json:"order_id"`
	Number  string `json:"number"`
	Total   int64  `json:"total"`
}

type readyResp struct {
	OrderID       int64  `json:"order_id"`
	ReadySeq      int64  `json:"ready_seq"`
	Number        string `json:"number"`
	NewlyAssigned bool   `json:"newly_assigned"`
}

func orderBody(ref string, item, qty int) string {
	return fmt.Sprintf(`{"client_ref":%q,"items":[{"menu_item_id":%d,"quantity":%d}]}`, ref, item, qty)
}

// holdMenuLock locks menu item id FOR UPDATE until the returned func runs,
// so an order for it waits on the lock.
func holdMenuLock(t *testing.T, pool *pgxpool.Pool, id int) func() {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `SELECT 1 FROM menu_items WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	done := false
	release := func() {
		if !done {
			done = true
			_ = tx.Rollback(context.Background())
		}
	}
	t.Cleanup(release)
	return release
}

// awaitLockWaiter waits until some backend waits on a lock.
func awaitLockWaiter(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no backend waiting on a lock")
}

func TestPodUIDHeader(t *testing.T) {
	e := newEnv(t, testConfig("v1"), nil)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/whoami"}, {"GET", "/healthz"}, {"GET", "/api/menu"},
		{"GET", "/"}, {"GET", "/no-such-page"}, {"POST", "/orders"},
	} {
		resp := e.do(t, c.method, c.path, "", "", nil)
		if got := resp.Header.Get("X-Koupa-Pod-UID"); got != "uid-test" {
			t.Errorf("%s %s: X-Koupa-Pod-UID = %q (status %d)", c.method, c.path, got, resp.StatusCode)
		}
	}
	var who map[string]string
	if resp := e.do(t, "GET", "/api/whoami", "", "", &who); resp.StatusCode != 200 {
		t.Fatalf("whoami status %d", resp.StatusCode)
	}
	if who["pod_uid"] != "uid-test" || who["pod_name"] != "pod-test" || who["version"] != "v1" {
		t.Errorf("whoami = %v", who)
	}
}

func TestRequestEndFields(t *testing.T) {
	e := newEnv(t, testConfig("v1"), nil)

	var o orderResp
	if resp := e.do(t, "POST", "/orders", "run1-a1-t1", orderBody("run1-c1", 1, 2), &o); resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if o.OrderID == 0 || len(o.Number) != 4 || o.Total != 130 {
		t.Errorf("order = %+v", o)
	}
	recv := awaitLog(t, e.log, applog.EvRequestReceived, "attempt", "run1-a1-t1")
	if recv["path"] != "/orders" || recv["pod_uid"] != "uid-test" || recv["ts"] == nil {
		t.Errorf("request_received = %v", recv)
	}
	end := awaitLog(t, e.log, applog.EvRequestEnd, "attempt", "run1-a1-t1")
	if end["status"] != float64(201) || end["path"] != "/orders" || end["write_err"] != "" ||
		end["flush_err"] != "" || end["ctx_canceled"] != false || end["tx_outcome"] != "committed" {
		t.Errorf("request_end = %v", end)
	}
	if s, _ := end["commit_sent_at"].(string); s == "" {
		t.Errorf("commit_sent_at missing: %v", end)
	} else if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
		t.Errorf("commit_sent_at %q: %v", s, err)
	}
	if _, ok := end["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms missing: %v", end)
	}

	// No transaction: empty outcome and commit time.
	e.do(t, "GET", "/api/menu", "run1-a2-t1", "", nil)
	end = awaitLog(t, e.log, applog.EvRequestEnd, "attempt", "run1-a2-t1")
	if end["status"] != float64(200) || end["tx_outcome"] != "" || end["commit_sent_at"] != "" {
		t.Errorf("menu request_end = %v", end)
	}

	// Client gives up while the order waits on a lock; the transaction
	// still commits and the log says the context was canceled.
	release := holdMenuLock(t, e.pool, 2)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", e.ts.URL+"/orders", stringReader(orderBody("run1-c3", 2, 1)))
	req.Header.Set("X-Koupa-Attempt", "run1-a3-t1")
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	awaitLockWaiter(t, e.pool)
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("canceled request returned no error")
	}
	release()
	end = awaitLog(t, e.log, applog.EvRequestEnd, "attempt", "run1-a3-t1")
	if end["ctx_canceled"] != true || end["tx_outcome"] != "committed" || end["status"] != float64(201) {
		t.Errorf("canceled request_end = %v", end)
	}
}

// countingTracer counts statements sent on a pool.
type countingTracer struct{ n atomic.Int64 }

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}
func (c *countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAuditCursorSingleStatement(t *testing.T) {
	base := testdb.New(t)
	e := newEnv(t, testConfig("v1"), base)
	// The audit requests run on a dedicated one-connection pool that
	// nothing else uses (the broadcaster polls on base) and that is warmed
	// up first, so every traced statement belongs to the audit request.
	pcfg := base.Config().Copy()
	tr := &countingTracer{}
	pcfg.ConnConfig.Tracer = tr
	pcfg.MaxConns = 1
	pcfg.MinConns = 1
	auditPool, err := pgxpool.NewWithConfig(context.Background(), pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(auditPool.Close)
	if err := auditPool.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	as := New(testConfig("v1"), Deps{Pool: auditPool, Broadcaster: e.srv.deps.Broadcaster, Log: quietLog()})
	as.started.Store(true)
	ats := httptest.NewServer(as.Handler())
	t.Cleanup(ats.Close)
	audit := &testEnv{srv: as, ts: ats, pool: auditPool, log: e.log, cfg: e.cfg}

	// Run "r_1" (with a LIKE metachar) and runs that must not match it.
	refs := []string{"r_1-a", "r_1-b", "r_1-c", "rx1-a", "r_10-a", "r_1"}
	ids := map[string]int64{}
	for _, ref := range refs {
		var o orderResp
		if resp := e.do(t, "POST", "/orders", "", orderBody(ref, 1, 1), &o); resp.StatusCode != 201 {
			t.Fatalf("order %s: %d", ref, resp.StatusCode)
		}
		ids[ref] = o.OrderID
	}
	var cur struct {
		Cursor int64 `json:"cursor"`
	}
	if resp := audit.do(t, "GET", "/api/audit?run=r_1&mode=cursor", "", "", &cur); resp.StatusCode != 200 || cur.Cursor != 0 {
		t.Fatalf("empty cursor: %d %+v", resp.StatusCode, cur)
	}
	var want int64
	for _, ref := range []string{"r_1-a", "rx1-a", "r_1-b", "r_10-a", "r_1"} {
		var r readyResp
		if resp := e.do(t, "POST", fmt.Sprintf("/orders/%d/ready", ids[ref]), "att-"+ref, "", &r); resp.StatusCode != 200 {
			t.Fatalf("ready %s: %d", ref, resp.StatusCode)
		}
		if ref == "r_1-b" || ref == "r_1-a" {
			want = r.ReadySeq
		}
	}
	tr.n.Store(0)
	if resp := audit.do(t, "GET", "/api/audit?run=r_1&mode=cursor", "", "", &cur); resp.StatusCode != 200 {
		t.Fatalf("cursor status %d", resp.StatusCode)
	}
	if cur.Cursor != want {
		t.Errorf("cursor = %d, want %d", cur.Cursor, want)
	}
	if n := tr.n.Load(); n != 1 {
		t.Errorf("cursor used %d statements, want 1", n)
	}

	var orders map[string][]int64
	audit.do(t, "GET", "/api/audit?run=r_1&mode=orders", "", "", &orders)
	if len(orders) != 3 || orders["r_1-a"][0] != ids["r_1-a"] || len(orders["r_1-c"]) != 1 {
		t.Errorf("orders = %v", orders)
	}
	var ready []struct {
		OrderID      int64   `json:"order_id"`
		ReadySeq     int64   `json:"ready_seq"`
		ReadyAttempt *string `json:"ready_attempt"`
	}
	audit.do(t, "GET", "/api/audit?run=r_1&mode=ready", "", "", &ready)
	if len(ready) != 2 || ready[0].OrderID != ids["r_1-a"] || ready[1].ReadySeq != want ||
		ready[0].ReadyAttempt == nil || *ready[0].ReadyAttempt != "att-r_1-a" {
		t.Errorf("ready = %+v", ready)
	}
	for _, q := range []string{"run=&mode=cursor", "run=r_1&mode=bogus", "mode=cursor"} {
		if resp := audit.do(t, "GET", "/api/audit?"+q, "", "", nil); resp.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestSoldOutEndpointV1Is404(t *testing.T) {
	pool := testdb.New(t)
	v1 := newEnv(t, testConfig("v1"), pool)
	if resp := v1.do(t, "POST", "/api/menu/1/available", "", `{"available":false}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("v1 status %d, want 404", resp.StatusCode)
	}

	v2 := newEnv(t, testConfig("v2"), pool)
	if resp := v2.do(t, "POST", "/api/menu/1/available", "", `{"available":false}`, nil); resp.StatusCode != 200 {
		t.Fatalf("v2 status %d", resp.StatusCode)
	}
	var menu []struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		Price     int64  `json:"price"`
		Available bool   `json:"available"`
	}
	v2.do(t, "GET", "/api/menu", "", "", &menu)
	if len(menu) != 4 || menu[0].Available || !menu[1].Available || menu[0].Price != 65 {
		t.Fatalf("menu = %+v", menu)
	}
	var er map[string]string
	if resp := v2.do(t, "POST", "/orders", "", orderBody("s-1", 1, 1), &er); resp.StatusCode != 422 || er["error"] == "" {
		t.Errorf("v2 sold-out order: %d %v", resp.StatusCode, er)
	}
	// v1 does not check availability.
	if resp := v1.do(t, "POST", "/orders", "", orderBody("s-2", 1, 1), nil); resp.StatusCode != 201 {
		t.Errorf("v1 order of unavailable item: %d", resp.StatusCode)
	}
	if resp := v2.do(t, "POST", "/api/menu/x/available", "", `{"available":false}`, nil); resp.StatusCode != 400 {
		t.Errorf("bad id: %d", resp.StatusCode)
	}
	if resp := v2.do(t, "POST", "/api/menu/1/available", "", `{}`, nil); resp.StatusCode != 400 {
		t.Errorf("missing available: %d", resp.StatusCode)
	}
}

// TestSoldOutGateFollowsMajor: the sold-out route and the 422 check follow
// the MAJOR of the build version (semver image tags), not an exact string.
func TestSoldOutGateFollowsMajor(t *testing.T) {
	for _, tc := range []struct {
		version string
		soldOut bool
	}{
		{"v1.2.0", false}, {"v1", false}, {"dev", false}, {"", false},
		{"v2.2.0", true}, {"v2", true}, {"v2.10.3", true}, {"v3.0.0", true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			pool := testdb.New(t)
			admin := newEnv(t, testConfig("v2.0.0"), pool)
			if resp := admin.do(t, "POST", "/api/menu/1/available", "", `{"available":false}`, nil); resp.StatusCode != 200 {
				t.Fatalf("admin: mark item 1 sold out: status %d", resp.StatusCode)
			}
			e := newEnv(t, testConfig(tc.version), pool)
			var who map[string]string
			e.do(t, "GET", "/api/whoami", "", "", &who)
			if who["version"] != tc.version {
				t.Errorf("whoami version %q, want the full %q", who["version"], tc.version)
			}
			wantRoute, wantOrder := http.StatusNotFound, http.StatusCreated
			if tc.soldOut {
				wantRoute, wantOrder = http.StatusOK, http.StatusUnprocessableEntity
			}
			if resp := e.do(t, "POST", "/api/menu/2/available", "", `{"available":true}`, nil); resp.StatusCode != wantRoute {
				t.Errorf("sold-out route: status %d, want %d", resp.StatusCode, wantRoute)
			}
			if resp := e.do(t, "POST", "/orders", "", orderBody("g-1", 1, 1), nil); resp.StatusCode != wantOrder {
				t.Errorf("order of a sold-out item: status %d, want %d", resp.StatusCode, wantOrder)
			}
		})
	}
}

// fakeBroadcaster lets a test control readiness.
type fakeBroadcaster struct {
	ready     chan struct{}
	listening atomic.Bool
}

func (f *fakeBroadcaster) Register(ctx context.Context, _ chan<- broadcast.ReadyEvent) (uint64, int64, error) {
	<-ctx.Done()
	return 0, 0, ctx.Err()
}
func (f *fakeBroadcaster) Unregister(uint64)                 {}
func (f *fakeBroadcaster) Overflowed(uint64) <-chan struct{} { return nil }
func (f *fakeBroadcaster) Ready() <-chan struct{}            { return f.ready }
func (f *fakeBroadcaster) Listening() bool                   { return f.listening.Load() }

func TestHealthzWaitsForListen(t *testing.T) {
	pool := testdb.New(t)
	cfg := testConfig("v1")

	// Real broadcaster, not yet running: not ready.
	b := broadcast.New(pool, listenDSN(t, pool), cfg.BroadcastPoll, quietLog(), nil)
	s := New(cfg, Deps{Pool: pool, Broadcaster: b, Log: quietLog(), Hooks: hooks.Noop{}})
	ts := newPlainServer(t, s)
	s.started.Store(true)
	if code := get(t, ts+"/healthz"); code != 503 {
		t.Fatalf("before LISTEN: %d, want 503", code)
	}
	runBroadcaster(t, b)
	deadline := time.Now().Add(waitTimeout)
	for get(t, ts+"/healthz") != 200 {
		if time.Now().After(deadline) {
			t.Fatal("healthz never became 200")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Each condition on its own.
	f := &fakeBroadcaster{ready: make(chan struct{})}
	s2 := New(cfg, Deps{Pool: pool, Broadcaster: f, Log: quietLog(), Hooks: hooks.Noop{}})
	ts2 := newPlainServer(t, s2)
	f.listening.Store(true)
	close(f.ready)
	if code := get(t, ts2+"/healthz"); code != 503 {
		t.Errorf("server not started: %d, want 503", code)
	}
	s2.started.Store(true)
	if code := get(t, ts2+"/healthz"); code != 200 {
		t.Errorf("all ok: %d, want 200", code)
	}
	f.listening.Store(false)
	if code := get(t, ts2+"/healthz"); code != 503 {
		t.Errorf("LISTEN lost: %d, want 503", code)
	}
	f.listening.Store(true)
	closed := testdb.New(t)
	closed.Close()
	s3 := New(cfg, Deps{Pool: closed, Broadcaster: f, Log: quietLog(), Hooks: hooks.Noop{}})
	s3.started.Store(true)
	if code := get(t, newPlainServer(t, s3)+"/healthz"); code != 503 {
		t.Errorf("ping failing: %d, want 503", code)
	}
}

func TestOrderErrorStatus(t *testing.T) {
	cfg := testConfig("v2")
	cfg.IdempotencyKey = true
	cfg.LockTimeout = 200 * time.Millisecond
	e := newEnv(t, cfg, nil)
	check := func(name, method, path, body string, want int) {
		t.Helper()
		var er map[string]string
		resp := e.do(t, method, path, "", body, &er)
		if resp.StatusCode != want || er["error"] == "" {
			t.Errorf("%s: %d %v, want %d with error", name, resp.StatusCode, er, want)
		}
	}
	check("bad json", "POST", "/orders", `{"items":`, 400)
	check("no items", "POST", "/orders", `{"client_ref":"e-1","items":[]}`, 400)
	check("unknown item", "POST", "/orders", orderBody("e-2", 99, 1), 400)
	check("unknown order", "POST", "/orders/999999/ready", "", 404)
	check("bad order id", "POST", "/orders/abc/ready", "", 400)

	var first, again orderResp
	if resp := e.do(t, "POST", "/orders", "", orderBody("e-3", 2, 1), &first); resp.StatusCode != 201 {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	if resp := e.do(t, "POST", "/orders", "", orderBody("e-3", 2, 1), &again); resp.StatusCode != 200 || again != first {
		t.Errorf("replay: %d %+v, want 200 %+v", resp.StatusCode, again, first)
	}
	check("conflict", "POST", "/orders", orderBody("e-3", 2, 2), 409)

	release := holdMenuLock(t, e.pool, 3)
	check("lock timeout", "POST", "/orders", orderBody("e-4", 3, 1), 503)
	release()
	end := find(e.log.lines(t), applog.EvRequestEnd, "status", float64(503))
	if len(end) != 1 || end[0]["tx_outcome"] != "rolled_back" {
		t.Errorf("lock-timeout request_end = %v", end)
	}

	var r1, r2 readyResp
	e.do(t, "POST", fmt.Sprintf("/orders/%d/ready", first.OrderID), "k-1", "", &r1)
	e.do(t, "POST", fmt.Sprintf("/orders/%d/ready", first.OrderID), "k-2", "", &r2)
	if !r1.NewlyAssigned || r2.NewlyAssigned || r1.ReadySeq != r2.ReadySeq || r1.Number != first.Number {
		t.Errorf("ready twice: %+v %+v", r1, r2)
	}
	var pending []orderResp
	e.do(t, "GET", "/api/orders?status=pending", "", "", &pending)
	for _, p := range pending {
		if p.OrderID == first.OrderID {
			t.Errorf("ready order still pending: %+v", pending)
		}
	}
	check("bad status", "GET", "/api/orders?status=ready", "", 400)
}

func TestPendingOrders(t *testing.T) {
	e := newEnv(t, testConfig("v1"), nil)
	var a, b orderResp
	e.do(t, "POST", "/orders", "", orderBody("p-1", 1, 1), &a)
	e.do(t, "POST", "/orders", "", orderBody("p-2", 4, 2), &b)
	var pending []orderResp
	if resp := e.do(t, "GET", "/api/orders?status=pending", "", "", &pending); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(pending) != 2 || pending[0] != a || pending[1] != b {
		t.Errorf("pending = %+v, want %+v %+v", pending, a, b)
	}
	var full []struct {
		Number    string `json:"number"`
		ClientRef string `json:"client_ref"`
		Items     []struct {
			Name     string `json:"name"`
			Quantity int    `json:"quantity"`
		} `json:"items"`
	}
	e.do(t, "GET", "/api/orders?status=pending", "", "", &full)
	if len(full) == 2 && (full[0].ClientRef != "p-1" || full[1].ClientRef != "p-2") {
		t.Errorf("pending client_ref = %q, %q; want p-1, p-2", full[0].ClientRef, full[1].ClientRef)
	}
	if len(full) != 2 || len(full[1].Items) != 1 || full[1].Items[0].Name != "冬瓜檸檬" || full[1].Items[0].Quantity != 2 {
		t.Errorf("pending items = %+v", full)
	}
}

func TestPendingOrdersRunFilter(t *testing.T) {
	e := newEnv(t, testConfig("v1"), nil)
	var a, b, c orderResp
	e.do(t, "POST", "/orders", "", orderBody("runA-c1", 1, 1), &a)
	e.do(t, "POST", "/orders", "", orderBody("runB-c1", 1, 1), &b)
	e.do(t, "POST", "/orders", "", orderBody("runA-c2", 1, 1), &c)
	var got []orderResp
	e.do(t, "GET", "/api/orders?status=pending&run=runA", "", "", &got)
	if len(got) != 2 || got[0] != a || got[1] != c {
		t.Errorf("run=runA pending = %+v, want %+v %+v", got, a, c)
	}
	e.do(t, "GET", "/api/orders?status=pending&run=run_", "", "", &got) // _ is escaped, not a wildcard
	if len(got) != 0 {
		t.Errorf("run=run_ matched %+v", got)
	}
}

func TestPagesPlaceholder(t *testing.T) {
	e := newEnv(t, testConfig("v1"), nil)
	for _, p := range []string{"/", "/kitchen", "/board"} {
		if resp := e.do(t, "GET", p, "", "", nil); resp.StatusCode != 200 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	if resp := e.do(t, "GET", "/nope", "", "", nil); resp.StatusCode != 404 {
		t.Errorf("/nope: %d", resp.StatusCode)
	}
}

func TestRejectedWriteNotStarted(t *testing.T) {
	e := newEnv(t, testConfig("v2"), nil)
	for i, c := range []struct{ path, body string }{
		{"/orders", `{"items":`},
		{"/orders/abc/ready", ""},
		{"/api/menu/x/available", `{"available":false}`},
		{"/api/menu/1/available", `{}`},
		{"/no-such-write", ""},
	} {
		att := fmt.Sprintf("rej-%d", i)
		resp := e.do(t, "POST", c.path, att, c.body, nil)
		if resp.StatusCode < 400 {
			t.Fatalf("%s: status %d", c.path, resp.StatusCode)
		}
		end := awaitLog(t, e.log, applog.EvRequestEnd, "attempt", att)
		if end["tx_outcome"] != "not_started" || end["commit_sent_at"] != "" {
			t.Errorf("%s: request_end = %v", c.path, end)
		}
	}
	// Reads have no transaction and keep an empty outcome.
	e.do(t, "GET", "/api/menu", "rd-1", "", nil)
	if end := awaitLog(t, e.log, applog.EvRequestEnd, "attempt", "rd-1"); end["tx_outcome"] != "" {
		t.Errorf("read request_end = %v", end)
	}
}

func TestReadyRequestEndFields(t *testing.T) {
	e := newEnv(t, testConfig("v1"), nil)
	var o orderResp
	e.do(t, "POST", "/orders", "o-1", orderBody("rf-1", 1, 1), &o)
	end := awaitLog(t, e.log, applog.EvRequestEnd, "attempt", "o-1")
	if end["order_id"] != float64(o.OrderID) || end["replayed"] != false {
		t.Errorf("order request_end = %v", end)
	}

	var r1 readyResp
	e.do(t, "POST", fmt.Sprintf("/orders/%d/ready", o.OrderID), "rdy-1", "", &r1)
	e.do(t, "POST", fmt.Sprintf("/orders/%d/ready", o.OrderID), "rdy-2", "", nil)
	first := awaitLog(t, e.log, applog.EvRequestEnd, "attempt", "rdy-1")
	again := awaitLog(t, e.log, applog.EvRequestEnd, "attempt", "rdy-2")
	for name, c := range map[string]struct {
		l     map[string]any
		newly bool
	}{"first": {first, true}, "again": {again, false}} {
		l := c.l
		if l["order_id"] != float64(o.OrderID) || l["ready_seq"] != float64(r1.ReadySeq) ||
			l["newly_assigned"] != c.newly || l["tx_outcome"] != "committed" {
			t.Errorf("%s ready request_end = %v", name, l)
		}
		if _, ok := l["lock_wait_ms"].(float64); !ok {
			t.Errorf("%s: lock_wait_ms missing: %v", name, l)
		}
	}
	// Unknown order: order_id from the path is still logged.
	e.do(t, "POST", "/orders/987654/ready", "rdy-3", "", nil)
	if l := awaitLog(t, e.log, applog.EvRequestEnd, "attempt", "rdy-3"); l["order_id"] != float64(987654) || l["status"] != float64(404) {
		t.Errorf("unknown order request_end = %v", l)
	}
}
