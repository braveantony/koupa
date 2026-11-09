package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrConflict means the client_ref was already used for a different
// request body (HTTP 409).
var ErrConflict = errors.New("client_ref reused with a different request")

// errKeyVanished means ON CONFLICT reported an existing key but the next
// statement could not read it: the cleanup deleted it in between. The
// attempt is rolled back and PlaceOrder starts again from step 1.
var errKeyVanished = errors.New("idempotency key vanished after conflict")

// maxPlaceAttempts bounds the step-3 retries when the key vanishes.
const maxPlaceAttempts = 3

// CleanupInterval is how often the app runs CleanupKeys.
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
	if !opts.Idempotency {
		var res PlaceResult
		txr, err := RunWriteTx(ctx, pool, txo, func(c context.Context, tx pgx.Tx) (err error) {
			if res.Order, err = createOrder(c, tx, req, opts); err != nil {
				return err
			}
			return runHookBeforeCommit(c, tx)
		})
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
				return runHookBeforeCommit(c, tx)
			case errors.Is(err, pgx.ErrNoRows):
				res.Order, err = readKey(c, tx, req.ClientRef, hash)
				res.Replayed = err == nil
				return err
			default:
				return err
			}
		})
		if !errors.Is(err, errKeyVanished) {
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
		return Order{}, errKeyVanished
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

func runHookBeforeCommit(ctx context.Context, tx pgx.Tx) error {
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
