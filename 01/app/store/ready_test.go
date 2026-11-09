package store

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"koupa/internal/testdb"
)

func makeOrder(t testing.TB, pool *pgxpool.Pool) Order {
	t.Helper()
	dctx, cancel := DBContext()
	defer cancel()
	var o Order
	_, err := RunWriteTx(dctx, pool, TxOptions{}, func(c context.Context, tx pgx.Tx) error {
		var err error
		o, err = CreateOrderTx(c, tx, OrderRequest{Items: []OrderItem{{MenuItemID: 1, Quantity: 1}}}, false)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func markReady(pool *pgxpool.Pool, id int64, attempt string, delay time.Duration) (ReadyResult, TxResult, error) {
	dctx, cancel := DBContext()
	defer cancel()
	return MarkReady(dctx, pool, id, attempt, delay)
}

func TestMarkReadyBasic(t *testing.T) {
	pool := testdb.New(t)
	o := makeOrder(t, pool)
	r, tr, err := markReady(pool, o.ID, "a1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !r.NewlyAssigned || r.ReadySeq != 1 || r.OrderID != o.ID || r.Number != o.Number || tr.Outcome != TxCommitted {
		t.Fatalf("result = %+v %+v, order %+v", r, tr, o)
	}
	var status string
	var at *time.Time
	if err := pool.QueryRow(ctx(t), `SELECT status, ready_at FROM orders WHERE id=$1`, o.ID).Scan(&status, &at); err != nil {
		t.Fatal(err)
	}
	if status != "ready" || at == nil {
		t.Fatalf("status=%q ready_at=%v", status, at)
	}
}

func TestMarkReadyNotFound(t *testing.T) {
	pool := testdb.New(t)
	_, tr, err := markReady(pool, 424242, "a", 0)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if tr.Outcome != TxRolledBack {
		t.Fatalf("outcome = %v", tr.Outcome)
	}
}

func TestReadyAttemptRecorded(t *testing.T) {
	pool := testdb.New(t)
	o := makeOrder(t, pool)
	r1, _, err := markReady(pool, o.ID, "attempt-A", 0)
	if err != nil || !r1.NewlyAssigned {
		t.Fatalf("first: %+v %v", r1, err)
	}
	r2, _, err := markReady(pool, o.ID, "attempt-B", 0)
	if err != nil {
		t.Fatal(err)
	}
	if r2.NewlyAssigned || r2.ReadySeq != r1.ReadySeq || r2.Number != o.Number {
		t.Fatalf("repeat = %+v, first = %+v", r2, r1)
	}
	var att string
	var seq int64
	if err := pool.QueryRow(ctx(t), `SELECT ready_attempt, ready_seq FROM orders WHERE id=$1`, o.ID).Scan(&att, &seq); err != nil {
		t.Fatal(err)
	}
	if att != "attempt-A" || seq != r1.ReadySeq {
		t.Fatalf("ready_attempt=%q seq=%d", att, seq)
	}
	// A repeat must not consume a sequence value.
	o2 := makeOrder(t, pool)
	r3, _, err := markReady(pool, o2.ID, "attempt-C", 0)
	if err != nil || r3.ReadySeq != r1.ReadySeq+1 {
		t.Fatalf("next seq = %+v %v, want %d", r3, err, r1.ReadySeq+1)
	}
}

func TestConcurrentReadySameOrder(t *testing.T) {
	pool := testdb.New(t)
	o := makeOrder(t, pool)

	lc, err := pgx.ConnectConfig(ctx(t), pool.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close(context.Background())
	if _, err := lc.Exec(ctx(t), `LISTEN order_ready`); err != nil {
		t.Fatal(err)
	}

	type out struct {
		r   ReadyResult
		err error
	}
	res := make([]out, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range res {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, _, err := markReady(pool, o.ID, []string{"x", "y"}[i], 0)
			res[i] = out{r, err}
		}()
	}
	close(start)
	wg.Wait()
	for _, x := range res {
		if x.err != nil {
			t.Fatal(x.err)
		}
	}
	if res[0].r.NewlyAssigned == res[1].r.NewlyAssigned {
		t.Fatalf("NewlyAssigned both %v", res[0].r.NewlyAssigned)
	}
	if res[0].r.ReadySeq != res[1].r.ReadySeq || res[0].r.ReadySeq != 1 {
		t.Fatalf("seqs %d, %d", res[0].r.ReadySeq, res[1].r.ReadySeq)
	}

	// Exactly one notification, with the seq as payload.
	wctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	n, err := lc.WaitForNotification(wctx)
	if err != nil {
		t.Fatal(err)
	}
	if n.Channel != "order_ready" || n.Payload != "1" {
		t.Fatalf("notification = %+v", n)
	}
	wctx2, cancel2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel2()
	if n2, err := lc.WaitForNotification(wctx2); err == nil {
		t.Fatalf("unexpected second notification %+v", n2)
	}
}

// TestCommitOrderMatchesSeq proves "smaller ready_seq commits first".
// Tx A takes the advisory lock and is held for 300ms (delayAfterLock). Tx B
// starts while A holds it; pg_locks confirms B is waiting on the advisory
// lock and B has not finished. The sequence value is drawn only after the
// lock is held and the lock is released only by A's commit, so B cannot
// draw its value before A has committed. The real server-side commit
// order is then read back with pg_xact_commit_timestamp.
func TestCommitOrderMatchesSeq(t *testing.T) {
	pool := testdb.New(t)
	oa, ob := makeOrder(t, pool), makeOrder(t, pool)

	type out struct {
		r   ReadyResult
		tr  TxResult
		err error
	}
	aCh, bCh := make(chan out, 1), make(chan out, 1)
	go func() {
		r, tr, err := markReady(pool, oa.ID, "a", 300*time.Millisecond)
		aCh <- out{r, tr, err}
	}()

	// Wait until A holds the advisory lock.
	waitFor(t, func() bool { return advisoryLocks(t, pool, true) == 1 })
	go func() {
		r, tr, err := markReady(pool, ob.ID, "b", 0)
		bCh <- out{r, tr, err}
	}()
	// B must wait on the advisory lock behind A.
	waitFor(t, func() bool { return advisoryLocks(t, pool, false) == 1 })
	select {
	case <-bCh:
		t.Fatal("B finished while A still held the lock")
	default:
	}

	a := <-aCh
	if a.err != nil {
		t.Fatal(a.err)
	}
	b := <-bCh
	if b.err != nil {
		t.Fatal(b.err)
	}
	if a.r.ReadySeq != 1 || b.r.ReadySeq != 2 {
		t.Fatalf("seqs A=%d B=%d, want 1 and 2", a.r.ReadySeq, b.r.ReadySeq)
	}
	if a.tr.Outcome != TxCommitted || b.tr.Outcome != TxCommitted {
		t.Fatalf("outcomes A=%v B=%v", a.tr.Outcome, b.tr.Outcome)
	}
	// Real commit order from the server: commit timestamps (the test
	// database runs with track_commit_timestamp=on).
	rows, err := pool.Query(ctx(t),
		`SELECT ready_seq, pg_xact_commit_timestamp(xmin) FROM orders WHERE id = ANY($1) ORDER BY ready_seq`,
		[]int64{oa.ID, ob.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var seqs []int64
	var cts []time.Time
	for rows.Next() {
		var s int64
		var ct *time.Time
		if err := rows.Scan(&s, &ct); err != nil {
			t.Fatal(err)
		}
		if ct == nil {
			t.Fatal("commit timestamp is NULL: start postgres with track_commit_timestamp=on (scripts/test-db.sh)")
		}
		seqs = append(seqs, s)
		cts = append(cts, *ct)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(cts) != 2 || !cts[0].Before(cts[1]) {
		t.Fatalf("commit timestamps by ready_seq %v = %v, want strictly increasing", seqs, cts)
	}
	t.Logf("ready_seq %v committed at %v", seqs, cts)
}

// TestReadyLockWait holds the advisory lock in another transaction for
// 200ms and checks that LockWait reports the wait.
func TestReadyLockWait(t *testing.T) {
	pool := testdb.New(t)
	oa, ob := makeOrder(t, pool), makeOrder(t, pool)
	aCh := make(chan error, 1)
	go func() {
		_, _, err := markReady(pool, oa.ID, "a", 200*time.Millisecond)
		aCh <- err
	}()
	waitFor(t, func() bool { return advisoryLocks(t, pool, true) == 1 })
	rb, _, err := markReady(pool, ob.ID, "b", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-aCh; err != nil {
		t.Fatal(err)
	}
	if rb.LockWait < 100*time.Millisecond || rb.LockWait > 5*time.Second {
		t.Fatalf("LockWait = %v, want about 200ms minus startup skew", rb.LockWait)
	}
	t.Logf("LockWait = %v", rb.LockWait)
}

func advisoryLocks(t testing.TB, pool *pgxpool.Pool, granted bool) int {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	if err := pool.QueryRow(c,
		`SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND granted=$1 AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`,
		granted).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitFor(t testing.TB, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// BenchmarkReadyUnderLoad marks 20 orders per second (one second per
// iteration), each from its own goroutine as the kitchen would, on the
// real path (no delay). It reports request latency and lock contention
// sampled from pg_locks. No pass threshold.
func BenchmarkReadyUnderLoad(b *testing.B) {
	pool := testdb.New(b)
	const perSec = 20
	var lat []time.Duration
	var mu sync.Mutex
	var waits []time.Duration
	var samples, contended, maxWaiters int

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		orders := make([]Order, perSec)
		for j := range orders {
			orders[j] = makeOrder(b, pool)
		}
		b.StartTimer()

		stop := make(chan struct{})
		sampDone := make(chan struct{})
		go func() {
			defer close(sampDone)
			for {
				select {
				case <-stop:
					return
				case <-time.After(2 * time.Millisecond):
				}
				w := advisoryLocks(b, pool, false)
				mu.Lock()
				samples++
				if w > 0 {
					contended++
				}
				if w > maxWaiters {
					maxWaiters = w
				}
				mu.Unlock()
			}
		}()

		var wg sync.WaitGroup
		tick := time.NewTicker(time.Second / perSec)
		for _, o := range orders {
			<-tick.C
			wg.Add(1)
			go func() {
				defer wg.Done()
				t0 := time.Now()
				r, _, err := markReady(pool, o.ID, "bench", 0)
				d := time.Since(t0)
				if err != nil {
					b.Error(err)
					return
				}
				mu.Lock()
				lat = append(lat, d)
				waits = append(waits, r.LockWait)
				mu.Unlock()
			}()
		}
		tick.Stop()
		wg.Wait()
		close(stop)
		<-sampDone
	}
	if len(lat) == 0 {
		return
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	var sum time.Duration
	for _, d := range lat {
		sum += d
	}
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	b.ReportMetric(ms(sum/time.Duration(len(lat))), "mean_ms")
	b.ReportMetric(ms(lat[len(lat)*99/100]), "p99_ms")
	b.ReportMetric(ms(lat[len(lat)-1]), "max_ms")
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	var wsum time.Duration
	for _, d := range waits {
		wsum += d
	}
	b.ReportMetric(ms(wsum/time.Duration(len(waits))), "lockwait_mean_ms")
	b.ReportMetric(ms(waits[len(waits)/2]), "lockwait_p50_ms")
	b.ReportMetric(ms(waits[len(waits)*99/100]), "lockwait_p99_ms")
	b.ReportMetric(ms(waits[len(waits)-1]), "lockwait_max_ms")
	if samples > 0 {
		b.ReportMetric(100*float64(contended)/float64(samples), "lockwait_sample_%")
	}
	b.ReportMetric(float64(maxWaiters), "max_lock_waiters")
}

func TestReadyAfterAndMax(t *testing.T) {
	pool := testdb.New(t)
	c := ctx(t)
	if m, err := MaxReadySeq(c, pool); err != nil || m != 0 {
		t.Fatalf("empty max = %d %v", m, err)
	}
	var want []ReadyEvent
	for i := 0; i < 5; i++ {
		o := makeOrder(t, pool)
		r, _, err := markReady(pool, o.ID, "a", 0)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, ReadyEvent{ReadySeq: r.ReadySeq, OrderID: o.ID, Number: o.Number})
	}
	makeOrder(t, pool) // not ready: must not appear
	if m, err := MaxReadySeq(c, pool); err != nil || m != 5 {
		t.Fatalf("max = %d %v", m, err)
	}
	got, err := ReadyAfter(c, pool, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != want[1] || got[1] != want[2] || got[2] != want[3] {
		t.Fatalf("ReadyAfter(1,3) = %+v, want %+v", got, want[1:4])
	}
	got, err = ReadyAfter(c, pool, 5, 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("ReadyAfter(5) = %+v %v", got, err)
	}
}
