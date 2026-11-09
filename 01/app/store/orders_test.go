package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"koupa/internal/testdb"
)

func TestCreateOrderComputesTotal(t *testing.T) {
	pool := testdb.New(t)
	var o Order
	_, err := RunWriteTx(ctx(t), pool, TxOptions{}, func(c context.Context, tx pgx.Tx) (err error) {
		o, err = CreateOrderTx(c, tx, OrderRequest{ClientRef: "r1", Items: []OrderItem{
			{MenuItemID: 2, Quantity: 2}, // 55 x 2
			{MenuItemID: 1, Quantity: 1}, // 65
		}}, true)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.Total != 175 || o.ID == 0 || len(o.Number) != 4 {
		t.Fatalf("unexpected order %+v", o)
	}
	var n int
	var price int
	if err := pool.QueryRow(ctx(t), `SELECT count(*), sum(unit_price) FROM order_items WHERE order_id = $1`, o.ID).Scan(&n, &price); err != nil {
		t.Fatal(err)
	}
	if n != 2 || price != 120 {
		t.Fatalf("items n=%d price=%d", n, price)
	}
	pending, err := ListPending(ctx(t), pool, 10)
	if err != nil || len(pending) != 1 || pending[0].ID != o.ID {
		t.Fatalf("pending %+v err %v", pending, err)
	}
	want := []PendingItem{{"滾動珍珠奶茶", 1}, {"Pod 沫紅茶", 2}}
	if len(pending[0].Items) != 2 || pending[0].Items[0] != want[0] || pending[0].Items[1] != want[1] ||
		pending[0].Number != o.Number || pending[0].Total != 175 {
		t.Fatalf("pending items %+v", pending[0])
	}
}

func TestCreateOrderInvalid(t *testing.T) {
	pool := testdb.New(t)
	for name, items := range map[string][]OrderItem{
		"empty":   nil,
		"qty0":    {{MenuItemID: 1, Quantity: 0}},
		"unknown": {{MenuItemID: 99, Quantity: 1}},
	} {
		_, err := RunWriteTx(ctx(t), pool, TxOptions{}, func(c context.Context, tx pgx.Tx) error {
			_, err := CreateOrderTx(c, tx, OrderRequest{Items: items}, true)
			return err
		})
		if !errors.Is(err, ErrInvalidOrder) {
			t.Fatalf("%s: got %v", name, err)
		}
	}
}

func TestSoldOutRejectedWhenChecking(t *testing.T) {
	pool := testdb.New(t)
	if _, err := SetAvailable(ctx(t), pool, []int64{3}, false); err != nil {
		t.Fatal(err)
	}
	_, err := RunWriteTx(ctx(t), pool, TxOptions{}, func(c context.Context, tx pgx.Tx) error {
		_, err := CreateOrderTx(c, tx, OrderRequest{Items: []OrderItem{{MenuItemID: 3, Quantity: 1}}}, true)
		return err
	})
	if !errors.Is(err, ErrSoldOut) {
		t.Fatalf("got %v", err)
	}
}

func TestSoldOutIgnoredWhenNotChecking(t *testing.T) {
	pool := testdb.New(t)
	if _, err := SetAvailable(ctx(t), pool, []int64{3}, false); err != nil {
		t.Fatal(err)
	}
	_, err := RunWriteTx(ctx(t), pool, TxOptions{}, func(c context.Context, tx pgx.Tx) error {
		_, err := CreateOrderTx(c, tx, OrderRequest{Items: []OrderItem{{MenuItemID: 3, Quantity: 1}}}, false)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSoldOutAndOrderSerialize(t *testing.T) {
	pool := testdb.New(t)
	held := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	releaseOnce := func() { once.Do(func() { close(release) }) }
	defer releaseOnce() // never leave the order tx blocked on any exit path
	orderDone := make(chan error, 1)
	var o Order
	go func() {
		_, err := RunWriteTx(ctx(t), pool, TxOptions{}, func(c context.Context, tx pgx.Tx) (err error) {
			o, err = CreateOrderTx(c, tx, OrderRequest{ClientRef: "s", Items: []OrderItem{{MenuItemID: 1, Quantity: 1}}}, true)
			close(held)
			<-release
			return err
		})
		orderDone <- err
	}()
	select {
	case <-held:
	case err := <-orderDone:
		t.Fatalf("order tx ended before holding the lock: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the order tx to hold FOR SHARE")
	}

	soldDone := make(chan error, 1)
	go func() {
		_, err := SetAvailable(ctx(t), pool, []int64{1}, false)
		soldDone <- err
	}()

	// Wait until the SetAvailable backend is blocked on a lock.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx(t), `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query ILIKE '%FOR UPDATE%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("SetAvailable never blocked on a lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-soldDone:
		t.Fatalf("SetAvailable finished while the order held FOR SHARE: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	releaseOnce()
	if err := <-orderDone; err != nil {
		t.Fatal(err)
	}
	if err := <-soldDone; err != nil {
		t.Fatal(err)
	}
	if o.ID == 0 {
		t.Fatal("order not created")
	}
	menu, err := ListMenu(ctx(t), pool)
	if err != nil {
		t.Fatal(err)
	}
	if menu[0].ID != 1 || menu[0].Available {
		t.Fatalf("item 1 should be unavailable: %+v", menu[0])
	}
}

func TestRequestHashOrderIndependent(t *testing.T) {
	a := RequestHash(OrderRequest{Items: []OrderItem{{1, 2}, {3, 1}}})
	b := RequestHash(OrderRequest{Items: []OrderItem{{3, 1}, {1, 2}}})
	c := RequestHash(OrderRequest{Items: []OrderItem{{3, 1}, {1, 3}}})
	if a != b {
		t.Fatal("order must not matter")
	}
	if a == c {
		t.Fatal("quantity must matter")
	}
	if len(a) != 64 {
		t.Fatalf("hash %q", a)
	}
}
