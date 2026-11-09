package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"koupa/app/applog"
	"koupa/internal/testdb"
	"koupa/testproxy"
)

var oneTea = []OrderItem{{MenuItemID: 1, Quantity: 1}}

func idemOpts() PlaceOptions {
	return PlaceOptions{Idempotency: true, CheckSoldOut: true, LockTimeout: 5 * time.Second}
}

func place(t *testing.T, pool *pgxpool.Pool, ref string, items []OrderItem, opts PlaceOptions) (PlaceResult, TxResult, error) {
	t.Helper()
	dctx, cancel := DBContext()
	defer cancel()
	return PlaceOrder(dctx, pool, OrderRequest{ClientRef: ref, Items: items}, opts)
}

func countKeys(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx(t), `SELECT count(*) FROM idempotency_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// setHook installs a package test seam and restores the previous value
// when the test ends.
func setHook[T any](t *testing.T, hook *T, f T) {
	t.Helper()
	prev := *hook
	*hook = f
	t.Cleanup(func() { *hook = prev })
}

// waitLockWaiters waits until at least n sessions of this test database are
// blocked on a heavyweight lock.
func waitLockWaiters(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got int
		if err := pool.QueryRow(ctx(t), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d sessions waiting on a lock, want %d", got, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// recv receives from ch or fails the test after 15 seconds, so a broken
// implementation cannot hang the suite.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// shortTxTimeoutPool sets transaction_timeout for role koupa in this test
// database only (a role-level SET would be cluster-wide) and returns a new
// pool whose connections all log in after the change.
func shortTxTimeoutPool(t *testing.T, p *pgxpool.Pool, timeout string) *pgxpool.Pool {
	t.Helper()
	c := ctx(t)
	admin, err := pgx.Connect(c, os.Getenv("KOUPA_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(c)
	db := pgx.Identifier{p.Config().ConnConfig.Database}.Sanitize()
	if _, err := admin.Exec(c, `ALTER ROLE koupa IN DATABASE `+db+` SET transaction_timeout = '`+timeout+`'`); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(c, p.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var got string
	if err := pool.QueryRow(c, `SHOW transaction_timeout`).Scan(&got); err != nil || got != timeout {
		t.Fatalf("transaction_timeout = %q (%v), want %q", got, err, timeout)
	}
	return pool
}

// A6 must-test 1: the same client_ref reaches two Pods at once.
func TestConcurrentSameClientRef(t *testing.T) {
	p := testdb.New(t)
	pools := make([]*pgxpool.Pool, 2)
	for i := range pools {
		pl, err := pgxpool.NewWithConfig(ctx(t), p.Config().Copy())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pl.Close)
		if err := pl.Ping(ctx(t)); err != nil { // log in before the race starts
			t.Fatal(err)
		}
		pools[i] = pl
	}
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		res   [2]PlaceResult
		errs  [2]error
	)
	for i := range pools {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			dctx, cancel := DBContext()
			defer cancel()
			res[i], _, errs[i] = PlaceOrder(dctx, pools[i], OrderRequest{ClientRef: "same", Items: oneTea}, idemOpts())
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("pod %d: %v", i, err)
		}
	}
	if res[0].Order.Number != res[1].Order.Number || res[0].Order.ID != res[1].Order.ID {
		t.Fatalf("different orders: %+v vs %+v", res[0], res[1])
	}
	if res[0].Replayed == res[1].Replayed {
		t.Fatalf("want exactly one replay: %+v vs %+v", res[0], res[1])
	}
	if n := countOrders(t, p, "same"); n != 1 {
		t.Fatalf("orders = %d, want 1", n)
	}
}

// A6 must-test 2: a retry sent while the first request is still in its
// transaction waits, then returns the same number.
func TestRetryWhileFirstInFlight(t *testing.T) {
	pool := testdb.New(t)
	inside := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unpause := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unpause) // runs before the pool closes, even on failure
	var calls atomic.Int32
	setHook(t, &hookBeforeCommit, func(context.Context, pgx.Tx) error {
		if calls.Add(1) == 1 {
			close(inside)
			<-release
		}
		return nil
	})

	type out struct {
		r   PlaceResult
		tx  TxResult
		err error
	}
	first := make(chan out, 1)
	go func() {
		r, tx, err := place(t, pool, "fly", oneTea, idemOpts())
		first <- out{r, tx, err}
	}()
	recv(t, inside, "first request inside its transaction")
	retry := make(chan out, 1)
	go func() {
		r, tx, err := place(t, pool, "fly", oneTea, idemOpts())
		retry <- out{r, tx, err}
	}()
	waitLockWaiters(t, pool, 1) // the retry is blocked on the in-flight key
	select {
	case o := <-retry:
		t.Fatalf("retry returned while the first was in flight: %+v", o)
	default:
	}
	unpause()

	a, b := recv(t, first, "first request"), recv(t, retry, "retry")
	if a.err != nil || b.err != nil {
		t.Fatalf("first err %v, retry err %v", a.err, b.err)
	}
	if a.r.Replayed || !b.r.Replayed || a.r.Order != b.r.Order {
		t.Fatalf("first %+v, retry %+v; want retry to replay the same order", a.r, b.r)
	}
	if a.tx.Outcome != TxCommitted {
		t.Fatalf("first outcome %q", a.tx.Outcome)
	}
	if n := countOrders(t, pool, "fly"); n != 1 {
		t.Fatalf("orders = %d, want 1", n)
	}
}

// A6 must-test 3: when the first request rolls back, the retry creates the
// order. Checked both after the rollback and while the retry was already
// waiting on the first request's key.
func TestRetryAfterRollbackCreates(t *testing.T) {
	t.Run("after", func(t *testing.T) {
		pool := testdb.New(t)
		_, tx, err := place(t, pool, "rb", []OrderItem{{MenuItemID: 99, Quantity: 1}}, idemOpts())
		if !errors.Is(err, ErrInvalidOrder) || tx.Outcome != TxRolledBack {
			t.Fatalf("first: %+v %v, want rolled back invalid order", tx, err)
		}
		if n := countKeys(t, pool); n != 0 {
			t.Fatalf("keys after rollback = %d, want 0", n)
		}
		r, _, err := place(t, pool, "rb", oneTea, idemOpts())
		if err != nil || r.Replayed || r.Order.ID == 0 {
			t.Fatalf("retry: %+v %v, want a new order", r, err)
		}
		if n := countOrders(t, pool, "rb"); n != 1 {
			t.Fatalf("orders = %d, want 1", n)
		}
	})
	t.Run("while waiting", func(t *testing.T) {
		pool := testdb.New(t)
		holder, err := pool.Begin(ctx(t))
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback(ctx(t))
		if _, err := holder.Exec(ctx(t), `SELECT id FROM menu_items WHERE id = 1 FOR UPDATE`); err != nil {
			t.Fatal(err)
		}
		firstErr := make(chan error, 1)
		go func() {
			o := idemOpts()
			o.LockTimeout = 700 * time.Millisecond
			_, _, err := place(t, pool, "rbw", oneTea, o)
			firstErr <- err
		}()
		waitLockWaiters(t, pool, 1) // first holds its key, waits on the menu row
		retry := make(chan PlaceResult, 1)
		retryErr := make(chan error, 1)
		go func() {
			r, _, err := place(t, pool, "rbw", oneTea, idemOpts())
			retry <- r
			retryErr <- err
		}()
		waitLockWaiters(t, pool, 2) // retry waits on the first's key
		if err := recv(t, firstErr, "first request"); !IsLockTimeout(err) {
			t.Fatalf("first err = %v, want lock timeout", err)
		}
		if err := holder.Rollback(ctx(t)); err != nil {
			t.Fatal(err)
		}
		r := recv(t, retry, "retry")
		if err := recv(t, retryErr, "retry error"); err != nil || r.Replayed || r.Order.ID == 0 {
			t.Fatalf("retry: %+v %v, want a new order", r, err)
		}
		if n := countOrders(t, pool, "rbw"); n != 1 {
			t.Fatalf("orders = %d, want 1", n)
		}
	})
}

// A6 must-test 4: same client_ref, different body: 409.
func TestDifferentBodyConflict(t *testing.T) {
	pool := testdb.New(t)
	if _, _, err := place(t, pool, "diff", oneTea, idemOpts()); err != nil {
		t.Fatal(err)
	}
	_, _, err := place(t, pool, "diff", []OrderItem{{MenuItemID: 1, Quantity: 2}}, idemOpts())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	// Item order does not matter: same body in another order replays.
	multi := []OrderItem{{MenuItemID: 1, Quantity: 1}, {MenuItemID: 2, Quantity: 3}}
	if _, _, err := place(t, pool, "multi", multi, idemOpts()); err != nil {
		t.Fatal(err)
	}
	r, _, err := place(t, pool, "multi", []OrderItem{multi[1], multi[0]}, idemOpts())
	if err != nil || !r.Replayed {
		t.Fatalf("reordered body: %+v %v, want replay", r, err)
	}
	if a, b := countOrders(t, pool, "diff"), countOrders(t, pool, "multi"); a != 1 || b != 1 {
		t.Fatalf("orders diff=%d multi=%d, want 1 each", a, b)
	}
}

// A6 must-test 5 (step 4): lock_timeout at any step returns a lock-timeout
// error (503) after the transaction rolled back.
func TestLockTimeout503(t *testing.T) {
	for _, step := range []string{"menu", "key"} {
		t.Run(step, func(t *testing.T) {
			pool := testdb.New(t)
			holder, err := pool.Begin(ctx(t))
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback(ctx(t))
			lockSQL := `SELECT id FROM menu_items WHERE id = 1 FOR UPDATE`
			if step == "key" { // an in-flight request holding the same key
				lockSQL = `INSERT INTO idempotency_keys (client_ref, request_hash) VALUES ('lt', 'x')`
			}
			if _, err := holder.Exec(ctx(t), lockSQL); err != nil {
				t.Fatal(err)
			}
			o := idemOpts()
			o.LockTimeout = 200 * time.Millisecond
			start := time.Now()
			_, tx, err := place(t, pool, "lt", oneTea, o)
			if !IsLockTimeout(err) || errors.Is(err, ErrConflict) {
				t.Fatalf("err = %v, want lock timeout", err)
			}
			if tx.Outcome != TxRolledBack || !tx.CommitSentAt.IsZero() {
				t.Fatalf("tx = %+v, want rolled_back", tx)
			}
			if d := time.Since(start); d > 3*time.Second {
				t.Fatalf("took %v", d)
			}
			if err := holder.Rollback(ctx(t)); err != nil {
				t.Fatal(err)
			}
			if n, k := countOrders(t, pool, "lt"), countKeys(t, pool); n != 0 || k != 0 {
				t.Fatalf("orders=%d keys=%d, want 0 (rolled back)", n, k)
			}
		})
	}
}

// A6 must-test 6 (step 3): the key is cleaned up between the conflict and
// the re-read; the request rolls back, retries from step 1 and succeeds.
func TestKeyCleanedBetweenConflictAndRead(t *testing.T) {
	pool := testdb.New(t)
	if _, _, err := place(t, pool, "gone", oneTea, idemOpts()); err != nil {
		t.Fatal(err)
	}
	var conflicts atomic.Int32
	setHook(t, &hookAfterKeyConflict, func(c context.Context, ref string) {
		if conflicts.Add(1) == 1 {
			if _, err := pool.Exec(c, `DELETE FROM idempotency_keys WHERE client_ref = $1`, ref); err != nil {
				t.Errorf("delete key: %v", err)
			}
		}
	})
	r, tx, err := place(t, pool, "gone", oneTea, idemOpts())
	if err != nil || tx.Outcome != TxCommitted || r.Replayed {
		t.Fatalf("got %+v %+v %v, want a fresh committed order", r, tx, err)
	}
	if n := conflicts.Load(); n != 1 {
		t.Fatalf("conflicts = %d, want 1 (then a clean insert on retry)", n)
	}
	var orderID int64
	if err := pool.QueryRow(ctx(t), `SELECT order_id FROM idempotency_keys WHERE client_ref = 'gone'`).Scan(&orderID); err != nil || orderID != r.Order.ID {
		t.Fatalf("key order_id = %d (%v), want %d", orderID, err, r.Order.ID)
	}
}

// The re-read keeps missing: give up after 1 + 3 retries (A6: 最多 3 次).
func TestKeyCleanedRetriesAreBounded(t *testing.T) {
	pool := testdb.New(t)
	var reads atomic.Int32
	setHook(t, &hookAfterKeyConflict, func(context.Context, string) { reads.Add(1) })
	setHook(t, &keyReadSQL, `SELECT request_hash, order_id, number, total FROM idempotency_keys k
		LEFT JOIN orders o ON o.id = k.order_id WHERE false AND k.client_ref = $1`)
	if _, _, err := place(t, pool, "loop", oneTea, idemOpts()); err != nil {
		t.Fatal(err)
	}
	_, tx, err := place(t, pool, "loop", oneTea, idemOpts())
	if !errors.Is(err, ErrKeyVanished) || tx.Outcome != TxRolledBack {
		t.Fatalf("got %+v %v, want ErrKeyVanished after rollback", tx, err)
	}
	if n := reads.Load(); n != 4 {
		t.Fatalf("attempts = %d, want 4 (first try + 3 retries)", n)
	}
}

// A6 must-test 7: with the switch off idempotency_keys is never touched and
// a retry creates a duplicate order.
func TestIdempotencyOffDuplicates(t *testing.T) {
	pool := testdb.New(t)
	setHook(t, &hookAfterKeyConflict, func(context.Context, string) { t.Error("key path used with idempotency off") })
	opts := PlaceOptions{CheckSoldOut: true, LockTimeout: time.Second}
	a, _, err := place(t, pool, "off", oneTea, opts)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := place(t, pool, "off", oneTea, opts)
	if err != nil {
		t.Fatal(err)
	}
	if a.Replayed || b.Replayed || a.Order.ID == b.Order.ID {
		t.Fatalf("a=%+v b=%+v, want two distinct new orders", a, b)
	}
	if n := countOrders(t, pool, "off"); n != 2 {
		t.Fatalf("orders = %d, want 2", n)
	}
	if k := countKeys(t, pool); k != 0 {
		t.Fatalf("idempotency_keys rows = %d, want 0", k)
	}
}

// DuplicateOrder (TEST_MUTATION) adds one extra order in the same
// transaction, bypassing idempotency_keys.
func TestDuplicateOrderMutation(t *testing.T) {
	pool := testdb.New(t)
	dup := func(ref string) bool { return ref == "dup" }
	opts := idemOpts()
	opts.DuplicateOrder = dup
	if _, _, err := place(t, pool, "dup", oneTea, opts); err != nil {
		t.Fatal(err)
	}
	if _, _, err := place(t, pool, "keep", oneTea, opts); err != nil {
		t.Fatal(err)
	}
	if a, b := countOrders(t, pool, "dup"), countOrders(t, pool, "keep"); a != 2 || b != 1 {
		t.Fatalf("orders dup=%d keep=%d, want 2 and 1", a, b)
	}
	if k := countKeys(t, pool); k != 2 {
		t.Fatalf("keys = %d, want 2 (one per client_ref)", k)
	}
	r, _, err := place(t, pool, "dup", oneTea, opts)
	if err != nil || !r.Replayed {
		t.Fatalf("replay: %+v %v", r, err)
	}
	if a := countOrders(t, pool, "dup"); a != 2 {
		t.Fatalf("replay added orders: %d", a)
	}
	opts.Idempotency = false
	if _, _, err := place(t, pool, "dup", oneTea, opts); err != nil {
		t.Fatal(err)
	}
	if a := countOrders(t, pool, "dup"); a != 4 {
		t.Fatalf("orders dup=%d, want 4 after an unprotected duplicated order", a)
	}
}

// Cleanup must-test 1: a key aged exactly 24h59m stays.
func TestCleanupKeeps24h59m(t *testing.T) {
	pool := testdb.New(t)
	for _, ref := range []string{"young", "old"} {
		if _, _, err := place(t, pool, ref, oneTea, idemOpts()); err != nil {
			t.Fatal(err)
		}
	}
	c := ctx(t)
	if _, err := pool.Exec(c, `UPDATE idempotency_keys SET created_at = now() - interval '24 hours 59 minutes' WHERE client_ref = 'young'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(c, `UPDATE idempotency_keys SET created_at = now() - interval '25 hours 1 minute' WHERE client_ref = 'old'`); err != nil {
		t.Fatal(err)
	}
	// A key without order_id is never cleaned, however old.
	if _, err := pool.Exec(c, `INSERT INTO idempotency_keys (client_ref, request_hash, created_at) VALUES ('pending', 'x', now() - interval '30 hours')`); err != nil {
		t.Fatal(err)
	}
	n, err := CleanupKeys(c, pool)
	if err != nil || n != 1 {
		t.Fatalf("deleted %d (%v), want 1", n, err)
	}
	rows, err := pool.Query(c, `SELECT client_ref FROM idempotency_keys ORDER BY client_ref`)
	if err != nil {
		t.Fatal(err)
	}
	left, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(left) != 2 || left[0] != "pending" || left[1] != "young" {
		t.Fatalf("left %v (%v), want [pending young]", left, err)
	}
}

// Cleanup must-test 2: an order transaction dragged close to
// transaction_timeout commits; its key is visible right away, and even 24
// hours after the commit the cleanup keeps it although created_at is the
// (earlier) transaction start.
func TestCleanupAfterSlowCommit(t *testing.T) {
	p := testdb.New(t)
	pool := shortTxTimeoutPool(t, p, "2s")
	setHook(t, &hookBeforeCommit, func(c context.Context, tx pgx.Tx) error {
		time.Sleep(1500 * time.Millisecond)
		return nil
	})
	r, tx, err := place(t, pool, "slow", oneTea, idemOpts())
	if err != nil || tx.Outcome != TxCommitted {
		t.Fatalf("slow order: %+v %v", tx, err)
	}
	c := ctx(t)
	var lag time.Duration
	var orderID int64
	if err := pool.QueryRow(c, `SELECT order_id, $1::timestamptz - created_at FROM idempotency_keys WHERE client_ref = 'slow'`,
		tx.CommitSentAt).Scan(&orderID, &lag); err != nil || orderID != r.Order.ID {
		t.Fatalf("key right after commit: order_id %d (%v), want %d", orderID, err, r.Order.ID)
	}
	if lag < time.Second {
		t.Fatalf("created_at only %v before COMMIT; the transaction was not slow", lag)
	}
	// Pretend 24 hours have passed since the commit.
	if _, err := pool.Exec(c, `UPDATE idempotency_keys SET created_at = created_at - interval '24 hours'`); err != nil {
		t.Fatal(err)
	}
	if n, err := CleanupKeys(c, pool); err != nil || n != 0 {
		t.Fatalf("cleanup deleted %d (%v), want 0", n, err)
	}
	if k := countKeys(t, pool); k != 1 {
		t.Fatalf("keys = %d, want 1", k)
	}
}

// Cleanup must-test 3: short statements alternating with idle time exceed
// transaction_timeout; the session is killed before COMMIT, the dead
// connection is dropped, and neither the order nor the key exists.
func TestTxTimeoutLeavesNoKeyNoOrder(t *testing.T) {
	p := testdb.New(t)
	pool := shortTxTimeoutPool(t, p, "2s")
	var pid uint32
	setHook(t, &hookBeforeCommit, func(c context.Context, tx pgx.Tx) error {
		pid = tx.Conn().PgConn().PID()
		for start := time.Now(); time.Since(start) < 3*time.Second; {
			if _, err := tx.Exec(c, `SELECT 1`); err != nil {
				return err
			}
			time.Sleep(400 * time.Millisecond)
		}
		return nil
	})
	_, tx, err := place(t, pool, "tto", oneTea, idemOpts())
	t.Logf("tx %+v err %v", tx, err)
	if err == nil || IsLockTimeout(err) || tx.Outcome != TxRolledBack || !tx.CommitSentAt.IsZero() {
		t.Fatalf("tx %+v err %v, want not committed before COMMIT was sent", tx, err)
	}
	if n, k := countOrders(t, pool, "tto"), countKeys(t, pool); n != 0 || k != 0 {
		t.Fatalf("orders=%d keys=%d, want 0", n, k)
	}
	for _, conn := range pool.AcquireAllIdle(ctx(t)) {
		if conn.Conn().PgConn().PID() == pid {
			t.Fatalf("killed connection (pid %d) is back in the pool", pid)
		}
		conn.Release()
	}
}

// Cleanup must-test 4: COMMIT really runs, then the connection breaks
// before the reply: commit_unknown. With the switch on, a retry with the
// same client_ref leaves exactly one order; with it off, a duplicate.
func TestCommitUnknownThenRetryOneOrder(t *testing.T) {
	for _, idem := range []bool{true, false} {
		name := map[bool]string{true: "on", false: "off"}[idem]
		t.Run(name, func(t *testing.T) {
			pool := testdb.New(t)
			cc := pool.Config().ConnConfig
			proxy := testproxy.NewTCP(net.JoinHostPort(cc.Host, strconv.Itoa(int(cc.Port))))
			defer proxy.Close()
			proxy.OnClientMessage(func(msg []byte) testproxy.Action {
				if isCommit(msg) {
					return testproxy.ForwardThenCutClient
				}
				return testproxy.Forward
			})
			u := url.URL{Scheme: "postgres", User: url.UserPassword(cc.User, cc.Password),
				Host: proxy.Addr(), Path: "/" + cc.Database, RawQuery: "sslmode=disable"}
			proxied, err := pgxpool.New(context.Background(), u.String())
			if err != nil {
				t.Fatal(err)
			}
			defer proxied.Close()

			opts := idemOpts()
			opts.Idempotency = idem
			_, tx, err := place(t, proxied, "cu", oneTea, opts)
			if err == nil || tx.Outcome != TxCommitUnknown || tx.CommitSentAt.IsZero() {
				t.Fatalf("tx %+v err %v, want commit_unknown", tx, err)
			}
			if !idem { // without the key there is nothing to wait on
				deadline := time.Now().Add(5 * time.Second)
				for countOrders(t, pool, "cu") != 1 {
					if time.Now().After(deadline) {
						t.Fatal("first order never committed on the server")
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
			r, tx, err := place(t, pool, "cu", oneTea, opts)
			if err != nil || tx.Outcome != TxCommitted {
				t.Fatalf("retry: %+v %v", tx, err)
			}
			want := map[bool]int{true: 1, false: 2}[idem]
			if r.Replayed != idem {
				t.Fatalf("retry replayed=%v, want %v", r.Replayed, idem)
			}
			if n := countOrders(t, pool, "cu"); n != want {
				t.Fatalf("orders = %d, want %d", n, want)
			}
		})
	}
}

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// events returns the decoded JSON log lines with the given message.
func (b *lockedBuffer) events(t *testing.T, msg string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(b.buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// RunKeyCleanup runs CleanupKeys every interval, logs each run, keeps going
// after an error, and returns when ctx is done.
func TestRunKeyCleanup(t *testing.T) {
	pool := testdb.New(t)
	c := ctx(t)
	if _, _, err := place(t, pool, "aged", oneTea, idemOpts()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(c, `UPDATE idempotency_keys SET created_at = now() - interval '26 hours'`); err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.Connect(c, os.Getenv("KOUPA_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(c)
	acfg := admin.Config().Copy()
	acfg.Database = pool.Config().ConnConfig.Database
	dbAdmin, err := pgx.ConnectConfig(c, acfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dbAdmin.Close(c)
	// Make the cleanup fail first: the table is temporarily missing.
	if _, err := dbAdmin.Exec(c, `ALTER TABLE idempotency_keys RENAME TO idempotency_keys_away`); err != nil {
		t.Fatal(err)
	}

	logs := &lockedBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	lctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunKeyCleanup(lctx, pool, 50*time.Millisecond, log)
	}()

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s; logs:\n%s", what, logs.String())
			}
		}
	}
	waitFor("an error run", func() bool {
		for _, m := range logs.events(t, applog.EvKeyCleanup) {
			if m["level"] == "ERROR" && m["err"] != nil {
				return true
			}
		}
		return false
	})
	if _, err := dbAdmin.Exec(c, `ALTER TABLE idempotency_keys_away RENAME TO idempotency_keys`); err != nil {
		t.Fatal(err)
	}
	waitFor("the aged key to be deleted", func() bool { return countKeys(t, pool) == 0 })
	waitFor("a run logging deleted=1", func() bool {
		for _, m := range logs.events(t, applog.EvKeyCleanup) {
			if m["level"] == "INFO" && m["deleted"] == float64(1) {
				return true
			}
		}
		return false
	})
	if n := countOrders(t, pool, "aged"); n != 1 {
		t.Fatalf("orders = %d; cleanup must only delete keys", n)
	}

	stop()
	recv(t, done, "RunKeyCleanup to return after cancel")
}

// pauseHooks pauses at one named point until released.
type pauseHooks struct {
	name    string
	at      chan struct{}
	release chan struct{}
}

func (p *pauseHooks) Pause(ctx context.Context, name, _ string) {
	if name != p.name {
		return
	}
	close(p.at)
	select {
	case <-p.release:
	case <-ctx.Done():
	}
}

// A12: order.before_commit pauses inside the transaction, so a second
// connection does not see the order yet.
func TestBeforeCommitPausesBeforeCommit(t *testing.T) {
	pool := testdb.New(t)
	ph := &pauseHooks{name: "order.before_commit", at: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	unpause := func() { once.Do(func() { close(ph.release) }) }
	t.Cleanup(unpause)
	opts := idemOpts()
	opts.Hooks = ph
	opts.AttemptID = "att-7"
	done := make(chan error, 1)
	go func() {
		_, _, err := place(t, pool, "pause-bc", oneTea, opts)
		done <- err
	}()
	recv(t, ph.at, "request paused")
	if n := countOrders(t, pool, "pause-bc"); n != 0 {
		t.Fatalf("orders visible while paused before commit = %d", n)
	}
	unpause()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := countOrders(t, pool, "pause-bc"); n != 1 {
		t.Fatalf("orders after release = %d", n)
	}
}

// A12: the duplicate is settled only after the outcome is known: rolled
// back gives committed=false, committed gives true.
func TestDuplicateDoneReportsOutcome(t *testing.T) {
	pool := testdb.New(t)
	var got []bool
	opts := idemOpts()
	opts.DuplicateOrder = func(string) bool { return true }
	opts.DuplicateDone = func(_ string, c bool) { got = append(got, c) }
	setHook(t, &hookBeforeCommit, func(context.Context, pgx.Tx) error { return errors.New("boom") })
	if _, _, err := place(t, pool, "dd-1", oneTea, opts); err == nil {
		t.Fatal("want error")
	}
	hookBeforeCommit = nil
	if _, _, err := place(t, pool, "dd-2", oneTea, opts); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("settled = %v, want [false true]", got)
	}
}

// The before_commit pause uses the request context: cancelling it ends the
// pause, and the order still commits.
func TestBeforeCommitPauseUsesRequestContext(t *testing.T) {
	pool := testdb.New(t)
	ph := &pauseHooks{name: "order.before_commit", at: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(ph.release) })
	rctx, cancel := context.WithCancel(context.Background())
	opts := idemOpts()
	opts.Hooks = ph
	opts.RequestCtx = rctx
	done := make(chan error, 1)
	go func() { _, _, err := place(t, pool, "rc-1", oneTea, opts); done <- err }()
	recv(t, ph.at, "request paused")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := countOrders(t, pool, "rc-1"); n != 1 {
		t.Fatalf("orders = %d", n)
	}
}
