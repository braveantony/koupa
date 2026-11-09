package load

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"syscall"
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
	var werr error
	for try := 1; try <= 1+c.Retries; try++ {
		rec := c.attempt(ctx, seq, try, ref, body, scheduled)
		if err := c.W.Write(rec); err != nil && werr == nil {
			werr = err
		}
		if !shouldRetry(rec) || ctx.Err() != nil {
			break
		}
	}
	return werr
}

func shouldRetry(r record.Attempt) bool {
	return !r.FullResponse || r.Status == nil || *r.Status/100 != 2
}

func (c *Customer) attempt(ctx context.Context, seq uint64, try int, ref string, body []byte, scheduled time.Time) record.Attempt {
	rec := record.Attempt{
		Run: c.Run, Kind: record.KindOrder, ClientRef: ref,
		Attempt: wire.NewAttemptID(c.Run, seq, try), Try: try, ScheduledAt: scheduled,
	}
	actx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodPost, c.Base+"/orders", bytes.NewReader(body))
	if err != nil {
		rec.SentAt = time.Now()
		rec.RespondedAt = rec.SentAt
		return rec
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(wire.HeaderAttempt, rec.Attempt)

	rec.SentAt = time.Now()
	rec.SchedLagMs = LagMs(scheduled, rec.SentAt)
	resp, err := c.Client.Do(req)
	if err != nil {
		rec.RespondedAt = time.Now()
		rec.ErrKind = classify(err)
		return rec
	}
	defer resp.Body.Close()
	st := resp.StatusCode
	rec.Status = &st
	rec.PodUID = resp.Header.Get(wire.HeaderPodUID)
	data, err := io.ReadAll(resp.Body)
	rec.RespondedAt = time.Now()
	if err != nil {
		rec.ErrKind = classify(err)
		return rec
	}
	rec.FullResponse = true
	if resp.StatusCode/100 == 2 {
		var out struct {
			OrderID int64 `json:"order_id"`
		}
		if json.Unmarshal(data, &out) == nil {
			if out.OrderID != 0 {
				id := out.OrderID
				rec.OrderID = &id
			}
		}
	}
	return rec
}

func classify(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return record.ErrTimeout
	case errors.Is(err, syscall.ECONNREFUSED):
		return record.ErrRefused
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return record.ErrReset
	}
	return ""
}
