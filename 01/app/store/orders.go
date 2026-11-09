package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrInvalidOrder means the request is malformed: no items, a quantity
	// below 1, or a menu item id that does not exist. It is a client error.
	ErrInvalidOrder = errors.New("invalid order")
	// ErrSoldOut means an ordered item is unavailable (only returned when
	// the caller asks CreateOrderTx to check).
	ErrSoldOut = errors.New("item sold out")
)

// OrderItem is one line of an order.
type OrderItem struct {
	MenuItemID int64
	Quantity   int
}

// OrderRequest is a customer order as submitted.
type OrderRequest struct {
	ClientRef string
	Items     []OrderItem
}

// Order is a created or pending order.
type Order struct {
	ID     int64
	Number string
	Total  int64
}

// RequestHash returns a SHA-256 hex digest of the items, independent of
// their order. ClientRef is deliberately not part of it.
func RequestHash(req OrderRequest) string {
	items := append([]OrderItem(nil), req.Items...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].MenuItemID != items[j].MenuItemID {
			return items[i].MenuItemID < items[j].MenuItemID
		}
		return items[i].Quantity < items[j].Quantity
	})
	h := sha256.New()
	for _, it := range items {
		fmt.Fprintf(h, "%d:%d;", it.MenuItemID, it.Quantity)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CreateOrderTx inserts an order inside tx. Lock order (plan A7): menu rows
// are locked first, ids ascending, with FOR SHARE, so a concurrent
// SetAvailable (FOR UPDATE) serializes with it. Prices are copied from the
// menu. With checkSoldOut, an unavailable item returns ErrSoldOut; a
// malformed request returns ErrInvalidOrder.
func CreateOrderTx(ctx context.Context, tx pgx.Tx, req OrderRequest, checkSoldOut bool) (Order, error) {
	if len(req.Items) == 0 {
		return Order{}, fmt.Errorf("%w: no items", ErrInvalidOrder)
	}
	seen := map[int64]bool{}
	var ids []int64
	for _, it := range req.Items {
		if it.Quantity < 1 {
			return Order{}, fmt.Errorf("%w: quantity %d", ErrInvalidOrder, it.Quantity)
		}
		if !seen[it.MenuItemID] {
			seen[it.MenuItemID] = true
			ids = append(ids, it.MenuItemID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	rows, err := tx.Query(ctx,
		`SELECT id, price, available FROM menu_items WHERE id = ANY($1) ORDER BY id FOR SHARE`, ids)
	if err != nil {
		return Order{}, err
	}
	type menuRow struct {
		price     int64
		available bool
	}
	menu := map[int64]menuRow{}
	for rows.Next() {
		var id int64
		var m menuRow
		if err := rows.Scan(&id, &m.price, &m.available); err != nil {
			rows.Close()
			return Order{}, err
		}
		menu[id] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Order{}, err
	}

	var total int64
	for _, it := range req.Items {
		m, ok := menu[it.MenuItemID]
		if !ok {
			return Order{}, fmt.Errorf("%w: unknown menu item %d", ErrInvalidOrder, it.MenuItemID)
		}
		if checkSoldOut && !m.available {
			return Order{}, fmt.Errorf("%w: menu item %d", ErrSoldOut, it.MenuItemID)
		}
		total += m.price * int64(it.Quantity)
	}

	var o Order
	var num int64
	var ref *string
	if req.ClientRef != "" {
		ref = &req.ClientRef
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO orders (number, total, client_ref) VALUES (nextval('order_number_seq'), $1, $2)
		 RETURNING id, number`, total, ref).Scan(&o.ID, &num); err != nil {
		return Order{}, err
	}
	o.Number = FormatNumber(num)
	o.Total = total
	for _, it := range req.Items {
		if _, err := tx.Exec(ctx,
			`INSERT INTO order_items (order_id, menu_item_id, quantity, unit_price) VALUES ($1, $2, $3, $4)`,
			o.ID, it.MenuItemID, it.Quantity, menu[it.MenuItemID].price); err != nil {
			return Order{}, err
		}
	}
	return o, nil
}

// PendingItem is one line of a pending order, as the kitchen needs it.
type PendingItem struct {
	Name     string
	Quantity int
}

// PendingOrder is an order still preparing, with its items.
type PendingOrder struct {
	Order
	ClientRef string // empty when the order was created without one
	Items     []PendingItem
}

// ListPending returns orders still preparing, oldest first, each with its
// items. One query: the limited order set is joined to its items.
func ListPending(ctx context.Context, pool *pgxpool.Pool, limit int) ([]PendingOrder, error) {
	rows, err := pool.Query(ctx, `
SELECT o.id, o.number, o.total, o.client_ref, m.name, i.quantity
FROM (SELECT id, number, total, client_ref FROM orders WHERE status = 'preparing' ORDER BY id LIMIT $1) o
LEFT JOIN order_items i ON i.order_id = o.id
LEFT JOIN menu_items m ON m.id = i.menu_item_id
ORDER BY o.id, m.id`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingOrder
	for rows.Next() {
		var o Order
		var num int64
		var ref, name *string
		var qty *int
		if err := rows.Scan(&o.ID, &num, &o.Total, &ref, &name, &qty); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != o.ID {
			o.Number = FormatNumber(num)
			po := PendingOrder{Order: o}
			if ref != nil {
				po.ClientRef = *ref
			}
			out = append(out, po)
		}
		if name != nil && qty != nil {
			last := &out[len(out)-1]
			last.Items = append(last.Items, PendingItem{Name: *name, Quantity: *qty})
		}
	}
	return out, rows.Err()
}
