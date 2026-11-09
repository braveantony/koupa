package load

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"koupa/internal/wire"
	"koupa/rush-hour/record"
)

// Customer places orders and records every attempt.
type Customer struct {
	Client  *http.Client
	Base    string
	Run     string
	Timeout time.Duration // per attempt; 0 means 5s
	Retries int           // retries after the first attempt (same client_ref and body)
	W       *record.Writer
	MenuIDs []int // menu item ids to order from; default 1..4
	Seed    int64 // RNG seed (deterministic for tests)

	once sync.Once
	mu   sync.Mutex
	rng  *rand.Rand
}

type orderItem struct {
	MenuItemID int `json:"menu_item_id"`
	Quantity   int `json:"quantity"`
}

type orderReq struct {
	ClientRef string      `json:"client_ref"`
	Items     []orderItem `json:"items"`
}

func (c *Customer) init() {
	c.once.Do(func() {
		c.rng = rand.New(rand.NewSource(c.Seed))
		if len(c.MenuIDs) == 0 {
			c.MenuIDs = []int{1, 2, 3, 4}
		}
		if c.Timeout == 0 {
			c.Timeout = 5 * time.Second
		}
	})
}

// randomItems picks a total of 1..4 cups spread over random menu items.
func (c *Customer) randomItems() []orderItem {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 1 + c.rng.Intn(4)
	qty := map[int]int{}
	var order []int
	for i := 0; i < total; i++ {
		id := c.MenuIDs[c.rng.Intn(len(c.MenuIDs))]
		if qty[id] == 0 {
			order = append(order, id)
		}
		qty[id]++
	}
	items := make([]orderItem, 0, len(order))
	for _, id := range order {
		items = append(items, orderItem{id, qty[id]})
	}
	return items
}

// Place sends order number seq, scheduled for `scheduled`. The first attempt
// is retried up to Retries times with the same client_ref and body unless
// it got a complete 2xx response (any other status, or no full response,
// counts as failed). Every attempt is recorded; a record-write error is
// returned (and stays in W.Err()).
func (c *Customer) Place(ctx context.Context, seq uint64, scheduled time.Time) error {
	c.init()
	ref := fmt.Sprintf("%s-c%d", c.Run, seq)
	body, _ := json.Marshal(orderReq{ClientRef: ref, Items: c.randomItems()})
	return sendWithRetry(c.W, c.Retries, func() bool { return ctx.Err() == nil }, func(try int) record.Attempt {
		rec := record.Attempt{
			Run: c.Run, Kind: record.KindOrder, ClientRef: ref,
			Attempt: wire.NewAttemptID(c.Run, seq, try), Try: try, ScheduledAt: scheduled,
		}
		return send(ctx, call{Client: c.Client, Timeout: c.Timeout, Method: http.MethodPost, URL: c.Base + "/orders", Body: body},
			rec, func(r *record.Attempt, data []byte) {
				var out struct {
					OrderID *int64 `json:"order_id"`
				}
				if json.Unmarshal(data, &out) == nil {
					r.OrderID = out.OrderID
				}
			})
	})
}
