package broadcast

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"koupa/app/store"
	"koupa/internal/testdb"
)

const waitTimeout = 5 * time.Second

// setup returns a fresh test database pool and a DSN for the LISTEN
// connection (same database, role koupa).
func setup(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool := testdb.New(t)
	cc := pool.Config().ConnConfig
	u, err := url.Parse(os.Getenv("KOUPA_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(cc.User, cc.Password)
	u.Path = "/" + cc.Database
	return pool, u.String()
}

func quietLog() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// markOne creates an order and marks it ready.
func markOne(t *testing.T, pool *pgxpool.Pool) store.ReadyResult {
	t.Helper()
	dctx, cancel := store.DBContext()
	defer cancel()
	var o store.Order
	if _, err := store.RunWriteTx(dctx, pool, store.TxOptions{}, func(c context.Context, tx pgx.Tx) error {
		var err error
		o, err = store.CreateOrderTx(c, tx, store.OrderRequest{Items: []store.OrderItem{{MenuItemID: 1, Quantity: 1}}}, false)
		return err
	}); err != nil {
		t.Error(err)
		return store.ReadyResult{}
	}
	r, _, err := store.MarkReady(dctx, pool, o.ID, "t", 0)
	if err != nil {
		t.Error(err)
	}
	return r
}

// start runs b until the test ends and waits for Ready.
func start(t *testing.T, b *Broadcaster) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(waitTimeout):
			t.Error("Run did not return after cancel")
		}
	})
	select {
	case <-b.Ready():
	case <-time.After(waitTimeout):
		t.Fatal("broadcaster not ready")
	}
}

// register registers ch on a started broadcaster.
func register(t *testing.T, b *Broadcaster, ch chan<- ReadyEvent) (uint64, int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	id, p, err := b.Register(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	return id, p
}

func recv(t *testing.T, ch <-chan ReadyEvent) ReadyEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(waitTimeout):
		t.Fatal("no event received in time")
		return ReadyEvent{}
	}
}

// traces collects the broadcaster's per-trigger cycle records.
type traces struct {
	mu sync.Mutex
	cs []cycle
}

func (tr *traces) add(c cycle) { tr.mu.Lock(); tr.cs = append(tr.cs, c); tr.mu.Unlock() }

func (tr *traces) snapshot() []cycle {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]cycle(nil), tr.cs...)
}

// await waits for the record of the cycle that dispatched seq. A cycle is
// recorded after its last page, so a subscriber may see the event first.
func (tr *traces) await(t *testing.T, seq int64) cycle {
	t.Helper()
	var c cycle
	waitFor(t, func() bool {
		var ok bool
		c, ok = tr.find(seq)
		return ok
	})
	return c
}

// find returns the cycle that dispatched seq.
func (tr *traces) find(seq int64) (cycle, bool) {
	for _, c := range tr.snapshot() {
		for _, s := range c.seqs {
			if s == seq {
				return c, true
			}
		}
	}
	return cycle{}, false
}

func (tr *traces) count(kind string) int {
	n := 0
	for _, c := range tr.snapshot() {
		if c.kind == kind {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestDispatchInSeqOrder(t *testing.T) {
	pool, dsn := setup(t)
	b := New(pool, dsn, 50*time.Millisecond, quietLog(), nil)
	b.pageSize = 3 // force paging
	start(t, b)

	const n = 20
	chA, chB := make(chan ReadyEvent, n), make(chan ReadyEvent, n)
	_, pa := register(t, b, chA)
	_, pb := register(t, b, chB)
	if pa != 0 || pb != 0 {
		t.Fatalf("progress = %d, %d, want 0", pa, pb)
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); markOne(t, pool) }()
	}
	wg.Wait()

	for _, ch := range []chan ReadyEvent{chA, chB} {
		for i := int64(1); i <= n; i++ {
			ev := recv(t, ch)
			if ev.ReadySeq != i || ev.OrderID == 0 || ev.Number == "" {
				t.Fatalf("event %d = %+v", i, ev)
			}
		}
	}
}

func TestReconnectListenDispatchesMissed(t *testing.T) {
	pool, dsn := setup(t)
	b := New(pool, dsn, time.Hour, quietLog(), nil) // no poll help
	b.pageSize = 3                                  // the catch-up spans three pages
	tr := &traces{}
	b.trace = tr.add
	pids := make(chan uint32, 4)
	b.listenHook = func(_ context.Context, c *pgx.Conn) error {
		pids <- c.PgConn().PID()
		return nil
	}
	reconnecting := make(chan struct{})
	release := make(chan struct{})
	b.connectHook = func(attempt int) {
		if attempt == 1 {
			close(reconnecting)
			<-release
		}
	}
	start(t, b)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) }) // runs before Run is cancelled

	ch := make(chan ReadyEvent, 10)
	register(t, b, ch)
	pid := <-pids

	admin, err := pgx.Connect(context.Background(), os.Getenv("KOUPA_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var ok bool
	if err := admin.QueryRow(context.Background(), `SELECT pg_terminate_backend($1)`, int32(pid)).Scan(&ok); err != nil || !ok {
		t.Fatalf("terminate %d: %v %v", pid, ok, err)
	}
	select {
	case <-reconnecting:
	case <-time.After(waitTimeout):
		t.Fatal("listener did not try to reconnect")
	}

	// Calls complete while the LISTEN connection is down.
	var seqs []int64
	for i := 0; i < 7; i++ {
		seqs = append(seqs, markOne(t, pool).ReadySeq)
	}
	select {
	case ev := <-ch:
		t.Fatalf("event dispatched while LISTEN was down: %+v", ev)
	default:
	}
	once.Do(func() { close(release) })

	select {
	case <-pids:
	case <-time.After(waitTimeout):
		t.Fatal("LISTEN not re-established")
	}
	var first cycle
	for i, want := range seqs {
		ev := recv(t, ch)
		if ev.ReadySeq != want {
			t.Fatalf("got %+v, want seq %d", ev, want)
		}
		c := tr.await(t, want)
		if c.kind != kindRelisten {
			t.Fatalf("seq %d dispatched by %+v, want %s", want, c, kindRelisten)
		}
		if i == 0 {
			first = c
		} else if !c.triggerAt.Equal(first.triggerAt) {
			t.Fatalf("seq %d dispatched by a later cycle; all pages must go out in the re-LISTEN cycle", want)
		}
	}
}

func TestMissedNotifyCaughtByPoll(t *testing.T) {
	const poll = 200 * time.Millisecond
	pool, dsn := setup(t)
	b := New(pool, dsn, poll, quietLog(), nil)
	tr := &traces{}
	b.trace = tr.add
	// Simulate a lost NOTIFY: the LISTEN connection stops listening.
	b.listenHook = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `UNLISTEN order_ready`)
		return err
	}
	start(t, b)
	ch := make(chan ReadyEvent, 10)
	register(t, b, ch)
	// Let the loop reach steady state (one poll cycle done).
	waitFor(t, func() bool { return tr.count(kindPoll) >= 1 })

	before := time.Now()
	r := markOne(t, pool)
	committed := time.Now()
	ev := recv(t, ch)
	received := time.Now()
	if ev.ReadySeq != r.ReadySeq {
		t.Fatalf("got %+v, want %d", ev, r.ReadySeq)
	}
	c := tr.await(t, r.ReadySeq)
	if c.kind != kindPoll {
		t.Fatalf("dispatched by %+v, want poll", c)
	}
	t.Logf("commit=%s trigger=+%s query_done=+%s dispatch_start=+%s received=+%s",
		committed.Format(time.RFC3339Nano), c.triggerAt.Sub(committed), c.queryDoneAt.Sub(committed),
		c.dispatchAt.Sub(committed), received.Sub(committed))
	if c.triggerAt.Before(before) || c.queryDoneAt.Before(c.triggerAt) || c.dispatchAt.Before(c.queryDoneAt) || received.Before(c.dispatchAt) {
		t.Fatalf("times out of order: before=%v %+v received=%v", before, c, received)
	}
	// It is the first poll after the commit: no earlier poll triggered in
	// (committed, c.triggerAt) and left it behind.
	for _, o := range tr.snapshot() {
		if o.kind == kindPoll && o.triggerAt.After(committed) && o.triggerAt.Before(c.triggerAt) {
			t.Fatalf("poll at %v after commit did not dispatch it; dispatched at %v", o.triggerAt, c.triggerAt)
		}
	}
	if gap := c.triggerAt.Sub(committed); gap > poll+100*time.Millisecond {
		t.Fatalf("dispatching poll fired %v after commit, poll interval %v", gap, poll)
	}
	if n := tr.count(kindNotify); n != 0 {
		t.Fatalf("%d notify cycles despite UNLISTEN", n)
	}
}

func TestOverflowMarksSubscriber(t *testing.T) {
	pool, dsn := setup(t)
	b := New(pool, dsn, 50*time.Millisecond, quietLog(), nil)
	start(t, b)

	slow := make(chan ReadyEvent, 1) // never read
	id, _ := register(t, b, slow)
	ov := b.Overflowed(id)
	for i := 0; i < 3; i++ {
		markOne(t, pool)
	}
	select {
	case <-ov:
	case <-time.After(waitTimeout):
		t.Fatal("Overflowed not closed")
	}

	// The broadcaster is not blocked: a new subscriber gets the next call.
	fast := make(chan ReadyEvent, 10)
	_, p := register(t, b, fast)
	r := markOne(t, pool)
	for want := p + 1; want <= r.ReadySeq; want++ {
		if ev := recv(t, fast); ev.ReadySeq != want {
			t.Fatalf("fast got %+v (progress %d), want seq %d", ev, p, want)
		}
	}
	if len(slow) != 1 {
		t.Fatalf("slow channel len %d, want 1", len(slow))
	}
	if ev := <-slow; ev.ReadySeq != 1 {
		t.Fatalf("slow kept %+v, want seq 1", ev)
	}
	b.Unregister(id)
	if b.Overflowed(id) != nil {
		t.Fatal("Overflowed of unregistered id should be nil")
	}
}

func TestBroadcasterStartsAtCurrentMax(t *testing.T) {
	pool, dsn := setup(t)
	for i := 0; i < 5; i++ {
		markOne(t, pool)
	}
	b := New(pool, dsn, 50*time.Millisecond, quietLog(), nil)
	tr := &traces{}
	b.trace = tr.add
	start(t, b)
	ch := make(chan ReadyEvent, 10)
	if _, p := register(t, b, ch); p != 5 {
		t.Fatalf("progress = %d, want 5", p)
	}
	r := markOne(t, pool)
	if ev := recv(t, ch); ev.ReadySeq != r.ReadySeq || r.ReadySeq != 6 {
		t.Fatalf("first event %+v, want 6", ev)
	}
	n := tr.count(kindPoll)
	waitFor(t, func() bool { return tr.count(kindPoll) >= n+3 })
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event %+v", ev)
	default:
	}
}

func TestDropFuncSkipsEvent(t *testing.T) {
	pool, dsn := setup(t)
	var dropID uint64
	var mu sync.Mutex
	drop := func(sub uint64, seq int64) bool {
		mu.Lock()
		defer mu.Unlock()
		return sub == dropID && seq == 2
	}
	b := New(pool, dsn, 50*time.Millisecond, quietLog(), drop)
	start(t, b)
	victim, other := make(chan ReadyEvent, 10), make(chan ReadyEvent, 10)
	id, _ := register(t, b, victim)
	mu.Lock()
	dropID = id
	mu.Unlock()
	register(t, b, other)
	for i := 0; i < 3; i++ {
		markOne(t, pool)
	}
	for _, want := range []int64{1, 3} {
		if ev := recv(t, victim); ev.ReadySeq != want {
			t.Fatalf("victim got %+v, want %d", ev, want)
		}
	}
	for _, want := range []int64{1, 2, 3} {
		if ev := recv(t, other); ev.ReadySeq != want {
			t.Fatalf("other got %+v, want %d", ev, want)
		}
	}
}

func TestRegisterWaitsForReady(t *testing.T) {
	pool, dsn := setup(t)
	for i := 0; i < 3; i++ {
		markOne(t, pool)
	}
	b := New(pool, dsn, 50*time.Millisecond, quietLog(), nil)
	release := make(chan struct{})
	b.connectHook = func(attempt int) {
		if attempt == 0 {
			<-release
		}
	}

	// Before Run: a Register whose ctx ends returns an error, registers nothing.
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := b.Register(short, make(chan ReadyEvent, 1)); err == nil {
		t.Fatal("Register before Ready returned no error")
	}

	type res struct {
		id   uint64
		prog int64
		err  error
	}
	ch := make(chan ReadyEvent, 10)
	got := make(chan res, 1)
	go func() {
		id, p, err := b.Register(context.Background(), ch)
		got <- res{id: id, prog: p, err: err}
	}()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	t.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(waitTimeout):
			t.Error("Run did not return")
		}
	})
	// LISTEN is held back: Register must still be blocked.
	select {
	case r := <-got:
		t.Fatalf("Register returned before Ready: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	var r res
	select {
	case r = <-got:
	case <-time.After(waitTimeout):
		t.Fatal("Register still blocked after Ready")
	}
	if r.err != nil || r.prog != 3 {
		t.Fatalf("Register = %+v, want progress 3", r)
	}
	select {
	case <-b.Ready():
	default:
		t.Fatal("Register returned but Ready not closed")
	}
	m := markOne(t, pool)
	if ev := recv(t, ch); ev.ReadySeq != m.ReadySeq || m.ReadySeq != 4 {
		t.Fatalf("got %+v, want seq 4", ev)
	}
	b.Unregister(r.id)
	b.mu.Lock()
	left := len(b.subs)
	b.mu.Unlock()
	if left != 0 {
		t.Fatalf("subs left: %d", left)
	}
}

func TestListeningTracksConnection(t *testing.T) {
	pool, dsn := setup(t)
	b := New(pool, dsn, time.Hour, quietLog(), nil)
	pids := make(chan uint32, 4)
	b.listenHook = func(_ context.Context, c *pgx.Conn) error {
		pids <- c.PgConn().PID()
		return nil
	}
	reconnecting := make(chan struct{})
	release := make(chan struct{})
	b.connectHook = func(attempt int) {
		if attempt == 1 {
			close(reconnecting)
			<-release
		}
	}
	if b.Listening() {
		t.Fatal("Listening before Run")
	}
	start(t, b)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	if !b.Listening() {
		t.Fatal("not Listening after Ready")
	}
	pid := <-pids

	admin, err := pgx.Connect(context.Background(), os.Getenv("KOUPA_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var ok bool
	if err := admin.QueryRow(context.Background(), `SELECT pg_terminate_backend($1)`, int32(pid)).Scan(&ok); err != nil || !ok {
		t.Fatalf("terminate %d: %v %v", pid, ok, err)
	}
	select {
	case <-reconnecting:
	case <-time.After(waitTimeout):
		t.Fatal("listener did not try to reconnect")
	}
	if b.Listening() {
		t.Fatal("still Listening while the LISTEN connection is down")
	}
	once.Do(func() { close(release) })
	select {
	case <-pids:
	case <-time.After(waitTimeout):
		t.Fatal("LISTEN not re-established")
	}
	waitFor(t, b.Listening)
}
