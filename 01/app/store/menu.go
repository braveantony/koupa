package store

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MenuItem is one row of the menu.
type MenuItem struct {
	ID        int64
	Name      string
	Price     int64
	Available bool
}

// ListMenu returns the whole menu ordered by id.
func ListMenu(ctx context.Context, pool *pgxpool.Pool) ([]MenuItem, error) {
	rows, err := pool.Query(ctx, `SELECT id, name, price, available FROM menu_items ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MenuItem
	for rows.Next() {
		var m MenuItem
		if err := rows.Scan(&m.ID, &m.Name, &m.Price, &m.Available); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetAvailable sets availability for ids. Rows are locked FOR UPDATE in
// ascending id order first (plan A7), then updated, so it cannot deadlock
// with CreateOrderTx's FOR SHARE.
func SetAvailable(ctx context.Context, pool *pgxpool.Pool, ids []int64, available bool) (TxResult, error) {
	sorted := append([]int64(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return RunWriteTx(ctx, pool, TxOptions{}, func(c context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(c, `SELECT id FROM menu_items WHERE id = ANY($1) ORDER BY id FOR UPDATE`, sorted)
		if err != nil {
			return err
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		_, err = tx.Exec(c, `UPDATE menu_items SET available = $2 WHERE id = ANY($1)`, sorted, available)
		return err
	})
}
