package store

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"koupa/app/applog"
	"koupa/app/config"
	"koupa/internal/testdb"
)

func dsnFor(p *pgxpool.Pool) string {
	cc := p.Config().ConnConfig
	u := url.URL{
		Scheme: "postgres", User: url.UserPassword(cc.User, cc.Password),
		Host: cc.Host + ":" + itoa(int(cc.Port)), Path: "/" + cc.Database,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestPoolAcceptsCorrectTimeout(t *testing.T) {
	p := testdb.New(t)
	var buf bytes.Buffer
	log := applog.New(&buf, "uid", "pod", "v1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, config.Config{DatabaseURL: dsnFor(p)}, log)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	var v string
	if err := pool.QueryRow(ctx, "SHOW transaction_timeout").Scan(&v); err != nil || v != "1min" {
		t.Fatalf("timeout = %q, %v", v, err)
	}
	if !strings.Contains(buf.String(), `"ok":true`) {
		t.Errorf("want an ok=true check log, got %q", buf.String())
	}
}

func TestPoolRejectsWrongTimeout(t *testing.T) {
	p := testdb.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Per-database role setting: affects only this test's database, not the
	// cluster-wide role default other parallel tests rely on.
	admin, err := pgx.Connect(ctx, os.Getenv("KOUPA_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	db := p.Config().ConnConfig.Database
	if _, err := admin.Exec(ctx, `ALTER ROLE koupa IN DATABASE `+pgx.Identifier{db}.Sanitize()+` SET transaction_timeout = '0'`); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	log := applog.New(&buf, "uid", "pod", "v1")
	pool, err := NewPool(ctx, config.Config{DatabaseURL: dsnFor(p)}, log)
	if err == nil {
		pool.Close()
		t.Fatal("NewPool should fail when transaction_timeout is wrong")
	}

	found := false
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if m["event"] == applog.EvTxTimeoutCheck && m["ok"] == false {
			found = true
		}
	}
	if !found {
		t.Errorf("no tx_timeout_check ok=false log in %q", buf.String())
	}
}
