package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"koupa/app/applog"
	"koupa/app/config"
)

// expectedTxTimeout is how PostgreSQL 17 renders transaction_timeout = '60s'.
const expectedTxTimeout = "1min"

// NewPool opens a pool that verifies transaction_timeout on every new
// connection (a wrong one is rejected and discarded) and acquires one
// connection before returning so the app refuses to start on failure.
func NewPool(ctx context.Context, cfg config.Config, log *slog.Logger) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	pcfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		var got string
		if err := c.QueryRow(ctx, "SHOW transaction_timeout").Scan(&got); err != nil {
			log.Error(applog.EvTxTimeoutCheck, "ok", false, "err", err.Error())
			return fmt.Errorf("check transaction_timeout: %w", err)
		}
		if got != expectedTxTimeout {
			log.Error(applog.EvTxTimeoutCheck, "ok", false, "got", got, "want", expectedTxTimeout)
			return fmt.Errorf("transaction_timeout is %q, want %q", got, expectedTxTimeout)
		}
		log.Info(applog.EvTxTimeoutCheck, "ok", true, "got", got)
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("initial connection: %w", err)
	}
	conn.Release()
	return pool, nil
}
