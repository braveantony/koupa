package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TxOutcome is how a write transaction ended, as recorded in the
// request_end log.
type TxOutcome string

const (
	TxCommitted     TxOutcome = "committed"      // COMMIT succeeded
	TxRolledBack    TxOutcome = "rolled_back"    // known not committed
	TxNotStarted    TxOutcome = "not_started"    // BEGIN never succeeded
	TxCommitUnknown TxOutcome = "commit_unknown" // COMMIT sent, no reply
)

// TxResult is the outcome of RunWriteTx. CommitSentAt is set just before
// COMMIT is sent and stays zero if COMMIT was never sent.
type TxResult struct {
	Outcome      TxOutcome
	CommitSentAt time.Time
}

// TxOptions configures a write transaction.
type TxOptions struct {
	// LockTimeout is applied with SET LOCAL lock_timeout. Zero means
	// DefaultLockTimeout.
	LockTimeout time.Duration
}

// DefaultLockTimeout is the lock_timeout used when TxOptions leaves it zero.
const DefaultLockTimeout = 5 * time.Second

// dbTimeout bounds every database operation of one request.
const dbTimeout = 60 * time.Second

// DBContext returns the context for a request's database work. It is
// derived from context.Background, not from the HTTP request, so a client
// that disconnects does not cancel a transaction half way.
func DBContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), dbTimeout)
}

// RunWriteTx runs fn in a Read Committed transaction on pool and reports
// how it ended:
//
//   - BEGIN (or acquiring a connection) fails: not_started.
//   - fn or SET LOCAL fails: rollback, rolled_back. This also covers a
//     session the server killed (transaction_timeout) or any connection
//     lost before COMMIT was sent: nothing can have committed.
//   - COMMIT returns a server error (or pgx reports the transaction was
//     already aborted): rolled_back.
//   - COMMIT fails any other way (connection lost, context deadline):
//     commit_unknown; the server may or may not have committed.
//
// A broken connection is never reused: pgx closes it on the I/O error and
// pgxpool destroys closed connections on release.
func RunWriteTx(ctx context.Context, pool *pgxpool.Pool, opts TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) (res TxResult, err error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TxResult{Outcome: TxNotStarted}, fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
	}()

	lt := opts.LockTimeout
	if lt <= 0 {
		lt = DefaultLockTimeout
	}
	// SET cannot take parameters; an integer millisecond count is safe to format.
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", lt.Milliseconds())); err != nil {
		return rollback(ctx, tx, fmt.Errorf("set lock_timeout: %w", err))
	}
	if err := fn(ctx, tx); err != nil {
		return rollback(ctx, tx, err)
	}

	res.CommitSentAt = time.Now()
	err = tx.Commit(ctx)
	switch {
	case err == nil:
		res.Outcome = TxCommitted
		return res, nil
	case commitDefinitelyFailed(err):
		res.Outcome = TxRolledBack
	default:
		res.Outcome = TxCommitUnknown
	}
	return res, fmt.Errorf("commit: %w", err)
}

// rollback ends a transaction that will not commit. A failing ROLLBACK (for
// example on a killed session) does not change the outcome; it is joined to
// the cause so callers still see both.
func rollback(ctx context.Context, tx pgx.Tx, cause error) (TxResult, error) {
	if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
		cause = errors.Join(cause, fmt.Errorf("rollback: %w", rbErr))
	}
	return TxResult{Outcome: TxRolledBack}, cause
}

// commitDefinitelyFailed reports whether a COMMIT error proves the
// transaction did not commit: the server answered with an error, or the
// transaction was already aborted. pgconn.SafeToRetry is deliberately not
// used: pgx v5.12 returns its "conn closed" lock error (SafeToRetry true)
// from a read after COMMIT was already written, which would hide exactly
// the commit_unknown case.
func commitDefinitelyFailed(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) || errors.Is(err, pgx.ErrTxCommitRollback)
}

// IsLockTimeout reports whether err is a lock_timeout cancellation (55P03).
func IsLockTimeout(err error) bool { return hasCode(err, "55P03") }

// IsDeadlock reports whether err is a detected deadlock (40P01).
func IsDeadlock(err error) bool { return hasCode(err, "40P01") }

func hasCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
