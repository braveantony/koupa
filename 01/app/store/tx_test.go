package store

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"koupa/internal/testdb"
	"koupa/testproxy"
)

func insertOrder(ref string) func(context.Context, pgx.Tx) error {
	return func(c context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(c, `INSERT INTO orders (number, total, client_ref) VALUES (1, 55, $1)`, ref)
		return err
	}
}

func countOrders(t *testing.T, pool *pgxpool.Pool, ref string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx(t), `SELECT count(*) FROM orders WHERE client_ref = $1`, ref).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCommitted(t *testing.T) {
	pool := testdb.New(t)
	dctx, cancel := DBContext()
	defer cancel()
	res, err := RunWriteTx(dctx, pool, TxOptions{LockTimeout: time.Second}, insertOrder("c1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != TxCommitted || res.CommitSentAt.IsZero() {
		t.Fatalf("result = %+v, want committed with CommitSentAt", res)
	}
	if n := countOrders(t, pool, "c1"); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

func TestRolledBackOnFnError(t *testing.T) {
	pool := testdb.New(t)
	dctx, cancel := DBContext()
	defer cancel()
	boom := errors.New("boom")
	res, err := RunWriteTx(dctx, pool, TxOptions{}, func(c context.Context, tx pgx.Tx) error {
		if err := insertOrder("r1")(c, tx); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if res.Outcome != TxRolledBack || !res.CommitSentAt.IsZero() {
		t.Fatalf("result = %+v, want rolled_back without CommitSentAt", res)
	}
	if n := countOrders(t, pool, "r1"); n != 0 {
		t.Fatalf("rows = %d, want 0", n)
	}
}

func TestNotStartedWhenBeginFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens here any more
	pool, err := pgxpool.New(context.Background(), "postgres://koupa:koupa@"+addr+"/x?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	dctx, cancel := DBContext()
	defer cancel()
	called := false
	res, err := RunWriteTx(dctx, pool, TxOptions{}, func(context.Context, pgx.Tx) error { called = true; return nil })
	if err == nil || res.Outcome != TxNotStarted || called {
		t.Fatalf("res=%+v err=%v called=%v, want not_started with error and fn not called", res, err, called)
	}
}

// isCommit reports whether a frontend message is the statement COMMIT, sent
// as a simple query ('Q': sql NUL) or a Parse ('P': name NUL sql NUL ...).
// It matches the whole statement, not a substring: BEGIN ISOLATION LEVEL
// READ COMMITTED also contains "commit".
func isCommit(msg []byte) bool {
	if len(msg) <= 5 || (msg[0] != 'Q' && msg[0] != 'P') {
		return false
	}
	fields := strings.Split(string(msg[5:]), "\x00")
	sql := fields[0]
	if msg[0] == 'P' {
		if len(fields) < 2 {
			return false
		}
		sql = fields[1]
	}
	return strings.EqualFold(strings.TrimRight(strings.TrimSpace(sql), "; "), "commit")
}

func TestCommitUnknownButCommitted(t *testing.T) {
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

	dctx, cancel := DBContext()
	defer cancel()
	res, err := RunWriteTx(dctx, proxied, TxOptions{LockTimeout: time.Second}, insertOrder("u1"))
	if err == nil {
		t.Fatal("want an error when the COMMIT reply is lost")
	}
	if res.Outcome != TxCommitUnknown || res.CommitSentAt.IsZero() {
		t.Fatalf("result = %+v (err %v), want commit_unknown with CommitSentAt", res, err)
	}
	// The server did run the COMMIT; poll briefly because it may land just after the cut.
	deadline := time.Now().Add(5 * time.Second)
	for countOrders(t, pool, "u1") != 1 {
		if time.Now().After(deadline) {
			t.Fatal("row not committed on the server")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestTransactionTimeoutKillsSession(t *testing.T) {
	p := testdb.New(t)
	c := ctx(t)
	admin, err := pgx.Connect(c, os.Getenv("KOUPA_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(c)
	db := p.Config().ConnConfig.Database
	if _, err := admin.Exec(c, `ALTER ROLE koupa IN DATABASE `+pgx.Identifier{db}.Sanitize()+` SET transaction_timeout = '2s'`); err != nil {
		t.Fatal(err)
	}
	// A fresh pool so every connection logs in after the change.
	cfg := p.Config().Copy()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var pid uint32
	dctx, cancel := DBContext()
	defer cancel()
	res, err := RunWriteTx(dctx, pool, TxOptions{LockTimeout: time.Second}, func(c context.Context, tx pgx.Tx) error {
		pid = tx.Conn().PgConn().PID()
		if err := insertOrder("t1")(c, tx); err != nil {
			return err
		}
		for start := time.Now(); time.Since(start) < 3*time.Second; {
			if _, err := tx.Exec(c, `SELECT 1`); err != nil {
				return err
			}
			time.Sleep(500 * time.Millisecond)
		}
		return nil
	})
	t.Logf("result %+v, err %v", res, err)
	if err == nil || res.Outcome != TxRolledBack || !res.CommitSentAt.IsZero() {
		t.Fatalf("res=%+v err=%v, want rolled_back with an error and no COMMIT sent", res, err)
	}
	if IsLockTimeout(err) {
		t.Fatalf("err is a lock timeout, want the session-kill error: %v", err)
	}
	if n := countOrders(t, p, "t1"); n != 0 {
		t.Fatalf("rows = %d, want 0", n)
	}
	// MaxConns=1: if the dead connection had gone back, we would get its PID.
	var next uint32
	if err := pool.QueryRow(c, `SELECT pg_backend_pid()`).Scan(&next); err != nil {
		t.Fatalf("pool unusable after the killed session: %v", err)
	}
	if next == pid {
		t.Fatalf("got the killed connection (pid %d) back from the pool", pid)
	}
}

func TestLockTimeoutRollsBack(t *testing.T) {
	pool := testdb.New(t)
	c := ctx(t)
	holder, err := pool.Begin(c)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(c)
	if _, err := holder.Exec(c, `SELECT id FROM menu_items WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	dctx, cancel := DBContext()
	defer cancel()
	start := time.Now()
	res, err := RunWriteTx(dctx, pool, TxOptions{LockTimeout: 200 * time.Millisecond}, func(c context.Context, tx pgx.Tx) error {
		if err := insertOrder("l1")(c, tx); err != nil {
			return err
		}
		_, err := tx.Exec(c, `SELECT id FROM menu_items WHERE id = 1 FOR UPDATE`)
		return err
	})
	if !IsLockTimeout(err) || IsDeadlock(err) {
		t.Fatalf("err = %v, want lock timeout", err)
	}
	if res.Outcome != TxRolledBack {
		t.Fatalf("outcome = %q, want rolled_back", res.Outcome)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v; lock_timeout not applied", d)
	}
	holder.Rollback(c)
	if n := countOrders(t, pool, "l1"); n != 0 {
		t.Fatalf("rows = %d, want 0", n)
	}
}

func TestDBContextIgnoresRequestCancel(t *testing.T) {
	pool := testdb.New(t)
	started := make(chan struct{})
	done := make(chan TxResult, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dctx, cancel := DBContext()
		defer cancel()
		res, _ := RunWriteTx(dctx, pool, TxOptions{}, func(c context.Context, tx pgx.Tx) error {
			if err := insertOrder("d1")(c, tx); err != nil {
				return err
			}
			close(started)
			select {
			case <-r.Context().Done(): // the client has gone away
				return nil
			case <-time.After(10 * time.Second):
				return errors.New("request context was never cancelled")
			}
		})
		done <- res
	}))
	defer srv.Close()

	rctx, rcancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(rctx, http.MethodPost, srv.URL, nil)
	go func() {
		<-started
		rcancel()
	}()
	client := &http.Client{Timeout: 15 * time.Second}
	if _, err := client.Do(req); err == nil {
		t.Fatal("request should have been cancelled")
	}
	select {
	case res := <-done:
		if res.Outcome != TxCommitted {
			t.Fatalf("outcome = %q, want committed", res.Outcome)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not finish")
	}
	if n := countOrders(t, pool, "d1"); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}

	dctx, cancel := DBContext()
	defer cancel()
	dl, ok := dctx.Deadline()
	if left := time.Until(dl); !ok || left < 59*time.Second || left > 60*time.Second {
		t.Fatalf("DBContext deadline in %v (ok=%v), want ~60s", left, ok)
	}
}

func TestCommitOutcomeClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want TxOutcome
	}{
		{"nil", nil, TxCommitted},
		{"serialization ERROR", &pgconn.PgError{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "40001"}, TxRolledBack},
		{"localized ERROR only", &pgconn.PgError{Severity: "ERROR", Code: "23505"}, TxRolledBack},
		{"already aborted", pgx.ErrTxCommitRollback, TxRolledBack},
		{"admin shutdown FATAL", &pgconn.PgError{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "57P01"}, TxCommitUnknown},
		{"localized FATAL", &pgconn.PgError{Severity: "SCHWERWIEGEND", SeverityUnlocalized: "FATAL", Code: "57P01"}, TxCommitUnknown},
		{"PANIC", &pgconn.PgError{Severity: "PANIC", SeverityUnlocalized: "PANIC", Code: "XX000"}, TxCommitUnknown},
		{"wrapped FATAL", fmt.Errorf("x: %w", &pgconn.PgError{SeverityUnlocalized: "FATAL", Code: "57P01"}), TxCommitUnknown},
		{"conn closed", pgconn.ErrConnClosed, TxCommitUnknown},
		{"deadline", context.DeadlineExceeded, TxCommitUnknown},
	}
	for _, c := range cases {
		if got := commitOutcome(c.err); got != c.want {
			t.Errorf("%s: commitOutcome = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSubMillisecondLockTimeoutRoundsUp(t *testing.T) {
	pool := testdb.New(t)
	dctx, cancel := DBContext()
	defer cancel()
	var got string
	_, err := RunWriteTx(dctx, pool, TxOptions{LockTimeout: 500 * time.Microsecond}, func(c context.Context, tx pgx.Tx) error {
		return tx.QueryRow(c, `SHOW lock_timeout`).Scan(&got)
	})
	if err != nil || got != "1ms" {
		t.Fatalf("lock_timeout = %q, %v; want 1ms (not 0 = disabled)", got, err)
	}
}
