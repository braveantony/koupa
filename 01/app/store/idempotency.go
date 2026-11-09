package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"koupa/app/applog"
	"koupa/app/hooks"
)

// ErrConflict means the client_ref was already used for a different
// request body (HTTP 409).
var ErrConflict = errors.New("client_ref reused with a different request")

// ErrKeyVanished means ON CONFLICT reported an existing key but the next
// statement could not read it: the cleanup deleted it in between. The
// attempt is rolled back and PlaceOrder starts again from step 1; it is
// returned only when every attempt hit it (HTTP 503, retryable).
var ErrKeyVanished = errors.New("idempotency key vanished after conflict")

// maxPlaceAttempts bounds PlaceOrder when the key vanishes: the first try
// plus at most 3 retries (plan A6 step 3).
const maxPlaceAttempts = 1 + 3

// CleanupInterval is how often the app runs CleanupKeys (via RunKeyCleanup).
const CleanupInterval = time.Hour

// PlaceOptions configures PlaceOrder.
type PlaceOptions struct {
	// Idempotency turns on the idempotency_keys protection (plan A6). Off,
	// the table is never touched and a retry creates a duplicate order.
	Idempotency bool
	// CheckSoldOut rejects unavailable items with ErrSoldOut.
	CheckSoldOut bool
	// LockTimeout is the transaction's lock_timeout; zero means
	// DefaultLockTimeout.
	LockTimeout time.Duration
	// DuplicateOrder is a deliberate bug for TEST_MUTATION; nil in normal
	// use. When it returns true for a request that creates an order, one
	// extra order is inserted in the same transaction, bypassing
	// idempotency_keys.
	DuplicateOrder func(clientRef string) bool
	// DuplicateDone settles a true DuplicateOrder: committed says whether the
	// transaction holding the extra order committed. nil in normal use.
	DuplicateDone func(clientRef string, committed bool)
	// RequestCtx is the request's context, used for the order.before_commit
	// pause (DB work keeps its own context); nil means the DB context.
	RequestCtx context.Context
	// Hooks is the test pause point (order.before_commit); nil means none.
	Hooks hooks.Point
	// AttemptID is the request's X-Koupa-Attempt, passed to Hooks.
	AttemptID string
}

// PlaceResult is the order a request ended with. Replayed means it was
// created by an earlier request with the same client_ref and body.
type PlaceResult struct {
	Order    Order
	Replayed bool
}

// Test seams; always nil (or the default) outside tests.
var (
	// hookAfterKeyConflict runs after ON CONFLICT DO NOTHING returned no
	// row and before the key is read back.
	hookAfterKeyConflict func(ctx context.Context, clientRef string)
	// hookBeforeCommit runs inside a transaction that created an order
	// (and filled in its key), just before RunWriteTx commits.
	hookBeforeCommit func(ctx context.Context, tx pgx.Tx) error
	// keyReadSQL reads an existing key and its order (step 3).
	keyReadSQL = `SELECT k.request_hash, k.order_id, o.number, o.total
		FROM idempotency_keys k LEFT JOIN orders o ON o.id = k.order_id
		WHERE k.client_ref = $1`
)

// PlaceOrder places an order in one Read Committed write transaction and
// returns how it ended (TxResult, for the request_end log).
//
// With opts.Idempotency (plan A6), lock order per A7:
//
//  1. INSERT the key ON CONFLICT DO NOTHING; this waits for an in-flight
//     transaction holding the same key.
//  2. Inserted: lock the menu, create the order, fill in order_id, commit.
//  3. Not inserted: read the key with the next statement. Same hash with an
//     order: return it (Replayed). Different hash: ErrConflict. Gone (just
//     cleaned up): roll back and start again, at most maxPlaceAttempts.
//
// Lock timeouts and deadlocks come back as errors after the rollback; the
// caller maps them with IsLockTimeout/IsDeadlock to 503, as it does for
// any other non-committed outcome.
func PlaceOrder(ctx context.Context, pool *pgxpool.Pool, req OrderRequest, opts PlaceOptions) (PlaceResult, TxResult, error) {
	txo := TxOptions{LockTimeout: opts.LockTimeout}
	// Track a reserved duplicate so it is settled once the outcome is known.
	var dupReserved bool
	if orig := opts.DuplicateOrder; orig != nil {
		opts.DuplicateOrder = func(ref string) bool {
			dupReserved = orig(ref)
			return dupReserved
		}
	}
	settle := func(ref string, txr TxResult, err error) {
		if dupReserved && opts.DuplicateDone != nil {
			opts.DuplicateDone(ref, err == nil && txr.Outcome == TxCommitted)
		}
		dupReserved = false
	}
	if !opts.Idempotency {
		var res PlaceResult
		txr, err := RunWriteTx(ctx, pool, txo, func(c context.Context, tx pgx.Tx) (err error) {
			if res.Order, err = createOrder(c, tx, req, opts); err != nil {
				return err
			}
			return runHookBeforeCommit(c, tx, opts)
		})
		settle(req.ClientRef, txr, err)
		if err != nil {
			return PlaceResult{}, txr, err
		}
		return res, txr, nil
	}

	if req.ClientRef == "" {
		return PlaceResult{}, TxResult{Outcome: TxNotStarted}, fmt.Errorf("%w: client_ref is required", ErrInvalidOrder)
	}
	hash := RequestHash(req)
	var (
		res PlaceResult
		txr TxResult
		err error
	)
	for attempt := 1; attempt <= maxPlaceAttempts; attempt++ {
		res = PlaceResult{}
		txr, err = RunWriteTx(ctx, pool, txo, func(c context.Context, tx pgx.Tx) error {
			var ref string
			err := tx.QueryRow(c,
				`INSERT INTO idempotency_keys (client_ref, request_hash, created_at) VALUES ($1, $2, now())
				 ON CONFLICT DO NOTHING RETURNING client_ref`, req.ClientRef, hash).Scan(&ref)
			switch {
			case err == nil:
				o, err := createOrder(c, tx, req, opts)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(c, `UPDATE idempotency_keys SET order_id = $2 WHERE client_ref = $1`, req.ClientRef, o.ID); err != nil {
					return err
				}
				res.Order = o
				return runHookBeforeCommit(c, tx, opts)
			case errors.Is(err, pgx.ErrNoRows):
				res.Order, err = readKey(c, tx, req.ClientRef, hash)
				res.Replayed = err == nil
				return err
			default:
				return err
			}
		})
		settle(req.ClientRef, txr, err)
		if !errors.Is(err, ErrKeyVanished) {
			break
		}
	}
	if err != nil {
		return PlaceResult{}, txr, err
	}
	return res, txr, nil
}

// readKey is step 3: the key exists (committed by another transaction), so
// a new statement in this Read Committed transaction sees it.
func readKey(ctx context.Context, tx pgx.Tx, clientRef, hash string) (Order, error) {
	if hookAfterKeyConflict != nil {
		hookAfterKeyConflict(ctx, clientRef)
	}
	var (
		gotHash string
		orderID *int64
		number  *int64
		total   *int64
	)
	err := tx.QueryRow(ctx, keyReadSQL, clientRef).Scan(&gotHash, &orderID, &number, &total)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrKeyVanished
	}
	if err != nil {
		return Order{}, err
	}
	if gotHash != hash {
		return Order{}, ErrConflict
	}
	if orderID == nil || number == nil || total == nil {
		// Keys are inserted and get their order_id in one transaction, so a
		// committed key without an order means the data was changed by hand.
		return Order{}, fmt.Errorf("idempotency key %q has no order", clientRef)
	}
	return Order{ID: *orderID, Number: FormatNumber(*number), Total: *total}, nil
}

// createOrder creates the order (and the TEST_MUTATION duplicate, if asked).
func createOrder(ctx context.Context, tx pgx.Tx, req OrderRequest, opts PlaceOptions) (Order, error) {
	o, err := CreateOrderTx(ctx, tx, req, opts.CheckSoldOut)
	if err != nil {
		return Order{}, err
	}
	if opts.DuplicateOrder != nil && opts.DuplicateOrder(req.ClientRef) {
		if _, err := CreateOrderTx(ctx, tx, req, opts.CheckSoldOut); err != nil {
			return Order{}, fmt.Errorf("duplicate order: %w", err)
		}
	}
	return o, nil
}

func runHookBeforeCommit(ctx context.Context, tx pgx.Tx, opts PlaceOptions) error {
	if opts.Hooks != nil {
		pc := opts.RequestCtx
		if pc == nil {
			pc = ctx
		}
		opts.Hooks.Pause(pc, hooks.OrderBeforeCommit, opts.AttemptID)
	}
	if hookBeforeCommit == nil {
		return nil
	}
	return hookBeforeCommit(ctx, tx)
}

// CleanupKeys deletes keys old enough that no retry can still arrive. Only
// keys with an order are deleted; created_at is the transaction start
// (now()), so the extra hour over 24 keeps every key at least 24 hours
// after its commit as long as transaction_timeout caps a transaction well
// below one hour.
func CleanupKeys(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx,
		`DELETE FROM idempotency_keys WHERE order_id IS NOT NULL AND created_at < now() - interval '25 hours'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// RunKeyCleanup calls CleanupKeys every interval until ctx is done. Each run
// is logged (key_cleanup with the deleted count, or the error at ERROR
// level); an error does not stop the loop.
func RunKeyCleanup(ctx context.Context, pool *pgxpool.Pool, interval time.Duration, log *slog.Logger) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		rctx, cancel := context.WithTimeout(ctx, dbTimeout)
		n, err := CleanupKeys(rctx, pool)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error(applog.EvKeyCleanup, "err", err.Error())
			continue
		}
		log.Info(applog.EvKeyCleanup, "deleted", n)
	}
}
