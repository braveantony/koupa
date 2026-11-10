package store

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// readyLockKey is the one advisory lock every path that marks an order
// ready takes (0x6B6F757061 = "koupa"). Holding it from sequence draw to
// COMMIT makes commit order equal ready_seq order.
const readyLockKey int64 = 0x6B6F757061

// ErrNotFound means the order does not exist.
var ErrNotFound = errors.New("not found")

// ReadyResult is the outcome of MarkReady. NewlyAssigned is false when the
// order was already ready and the original ReadySeq is returned.
type ReadyResult struct {
	OrderID       int64
	ReadySeq      int64
	Number        string
	NewlyAssigned bool
	// LockWait is how long taking the advisory lock took (zero when the
	// order was already ready and the lock was not needed).
	LockWait time.Duration
}

// MarkReady marks an order ready and assigns its call-number (plan A1):
//
//  1. lock the order row; if already ready, return the original result
//     with no new number and no NOTIFY;
//  2. take the advisory lock readyLockKey;
//  3. draw ready_seq, set status, ready_seq, ready_at, ready_attempt;
//  4. NOTIFY order_ready with the ready_seq as payload;
//  5. COMMIT.
//
// delayAfterLock sleeps while holding the advisory lock and exists only
// for tests; production callers pass 0. Nothing slow happens after the
// lock is taken. A missing order returns ErrNotFound.
func MarkReady(ctx context.Context, pool *pgxpool.Pool, orderID int64, attemptID string, delayAfterLock time.Duration) (ReadyResult, TxResult, error) {
	var res ReadyResult
	tr, err := RunWriteTx(ctx, pool, TxOptions{}, func(c context.Context, tx pgx.Tx) error {
		res = ReadyResult{}
		var num int64
		var status string
		var seq *int64
		err := tx.QueryRow(c,
			`SELECT number, status, ready_seq FROM orders WHERE id = $1 FOR UPDATE`, orderID).Scan(&num, &status, &seq)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		res.OrderID = orderID
		res.Number = FormatNumber(num)
		if status == "ready" && seq != nil {
			res.ReadySeq = *seq
			return nil
		}

		lockStart := time.Now() // monotonic
		if _, err := tx.Exec(c, `SELECT pg_advisory_xact_lock($1)`, readyLockKey); err != nil {
			return err
		}
		res.LockWait = time.Since(lockStart)
		if delayAfterLock > 0 {
			select {
			case <-time.After(delayAfterLock):
			case <-c.Done():
				return c.Err()
			}
		}
		// ready_at uses clock_timestamp(): now() would be the transaction
		// start, before the lock wait.
		if err := tx.QueryRow(c,
			`UPDATE orders SET status = 'ready', ready_seq = nextval('ready_seq_seq'),
			        ready_at = clock_timestamp(), ready_attempt = $2
			 WHERE id = $1 RETURNING ready_seq`, orderID, attemptID).Scan(&res.ReadySeq); err != nil {
			return err
		}
		if _, err := tx.Exec(c, `SELECT pg_notify('order_ready', $1)`, strconv.FormatInt(res.ReadySeq, 10)); err != nil {
			return err
		}
		res.NewlyAssigned = true
		return nil
	})
	if err != nil {
		return ReadyResult{}, tr, err
	}
	return res, tr, nil
}

// ReadyEvent is one call-number as the broadcaster dispatches it.
type ReadyEvent struct {
	ReadySeq int64
	OrderID  int64
	Number   string
}

// MaxReadySeq returns the largest ready_seq, or 0 when nothing is ready.
func MaxReadySeq(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var m int64
	err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(ready_seq), 0) FROM orders`).Scan(&m)
	return m, err
}

// ReadyAfter returns up to limit ready orders with ready_seq > after, in
// ascending ready_seq order.
func ReadyAfter(ctx context.Context, pool *pgxpool.Pool, after int64, limit int) ([]ReadyEvent, error) {
	rows, err := pool.Query(ctx,
		`SELECT ready_seq, id, number FROM orders
		 WHERE ready_seq > $1 ORDER BY ready_seq LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReadyEvent
	for rows.Next() {
		var ev ReadyEvent
		var num int64
		if err := rows.Scan(&ev.ReadySeq, &ev.OrderID, &num); err != nil {
			return nil, err
		}
		ev.Number = FormatNumber(num)
		out = append(out, ev)
	}
	return out, rows.Err()
}
