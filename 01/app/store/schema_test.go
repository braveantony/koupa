package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"koupa/internal/testdb"
	"koupa/internal/wire"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestReadySeqSequenceOptions(t *testing.T) {
	pool := testdb.New(t)
	var cache int64
	var cycle bool
	var start int64
	err := pool.QueryRow(ctx(t), `SELECT cache_size, cycle, start_value FROM pg_sequences WHERE sequencename = 'ready_seq_seq'`).Scan(&cache, &cycle, &start)
	if err != nil {
		t.Fatal(err)
	}
	if cache != 1 || cycle || start != 1 {
		t.Fatalf("cache=%d cycle=%v start=%d, want 1,false,1", cache, cycle, start)
	}
}

func TestOrderNumberWrapsTo0001(t *testing.T) {
	pool := testdb.New(t)
	c := ctx(t)
	if _, err := pool.Exec(c, `SELECT setval('order_number_seq', 9999)`); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := pool.QueryRow(c, `SELECT nextval('order_number_seq')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("nextval after 9999 = %d, want 1", n)
	}
	if got := FormatNumber(n); got != "0001" {
		t.Fatalf("FormatNumber(%d) = %q, want 0001", n, got)
	}
}

func TestClientRefNotUnique(t *testing.T) {
	pool := testdb.New(t)
	c := ctx(t)
	for i := 0; i < 2; i++ {
		_, err := pool.Exec(c, `INSERT INTO orders (number, total, client_ref) VALUES ($1, 55, 'same-ref')`, i+1)
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	var n int
	if err := pool.QueryRow(c, `SELECT count(*) FROM orders WHERE client_ref = 'same-ref'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("count=%d err=%v, want 2", n, err)
	}
	// idempotency_keys.client_ref is the primary key, so it must be unique.
	if _, err := pool.Exec(c, `INSERT INTO idempotency_keys (client_ref, request_hash) VALUES ('k', 'h')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(c, `INSERT INTO idempotency_keys (client_ref, request_hash) VALUES ('k', 'h')`); err == nil {
		t.Fatal("duplicate idempotency key accepted")
	}
}

// TestRoleTransactionTimeout only reads the role setting; it is cluster-wide.
func TestRoleTransactionTimeout(t *testing.T) {
	pool := testdb.New(t)
	c := ctx(t)
	cfg := pool.Config().ConnConfig
	conn, err := pgx.ConnectConfig(c, cfg) // fresh login as koupa
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(c)
	var v string
	if err := conn.QueryRow(c, `SHOW transaction_timeout`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != "1min" {
		t.Fatalf("transaction_timeout = %q, want 1min", v)
	}
}

func TestMenuSeeded(t *testing.T) {
	pool := testdb.New(t)
	rows, err := pool.Query(ctx(t), `SELECT name FROM menu_items ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"滾動珍珠奶茶", "Pod 沫紅茶", "微服務綠茶", "容器冬瓜茶"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestReadyAttemptStoresAttemptID(t *testing.T) {
	pool := testdb.New(t)
	c := ctx(t)
	want := wire.NewAttemptID("r1", 7, 1)
	var id int64
	if err := pool.QueryRow(c, `INSERT INTO orders (number, total, ready_attempt) VALUES (1, 55, $1) RETURNING id`, want).Scan(&id); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := pool.QueryRow(c, `SELECT ready_attempt FROM orders WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ready_attempt = %q, want %q", got, want)
	}
}
