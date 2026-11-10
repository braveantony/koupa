// Package testdb gives each test its own PostgreSQL database, cloned from the
// migrated koupa_template database on the server named by KOUPA_TEST_DSN.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	templateDB = "koupa_template"
	appRole    = "koupa"
	appRolePW  = "koupa"
)

// New clones koupa_template into a fresh database and returns a pool that
// connects to it as role koupa. The database is dropped when the test ends.
// With -short the test is skipped; without KOUPA_TEST_DSN it fails.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping database test in -short mode")
	}
	dsn := os.Getenv("KOUPA_TEST_DSN")
	if dsn == "" {
		t.Fatal("KOUPA_TEST_DSN is not set; run: eval \"$(scripts/test-db.sh)\"")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse KOUPA_TEST_DSN: %v", err)
	}
	admin, err := pgx.ConnectConfig(ctx, adminCfg)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(ctx)

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	name := "koupa_test_" + hex.EncodeToString(b[:])

	// name is generated above (hex only), so quoting it directly is safe.
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name+` TEMPLATE `+templateDB+` OWNER `+appRole); err != nil {
		t.Fatalf("clone template: %v", err)
	}

	// Register the drop right away so any later failure still removes the database.
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		c, err := pgx.ConnectConfig(cctx, adminCfg)
		if err != nil {
			t.Errorf("cleanup connect: %v", err)
			return
		}
		defer c.Close(cctx)
		if _, err := c.Exec(cctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.Database = name
	poolCfg.ConnConfig.User = appRole
	poolCfg.ConnConfig.Password = appRolePW
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	// Cleanups run LIFO: the pool closes before the database is dropped.
	t.Cleanup(pool.Close)
	return pool
}
