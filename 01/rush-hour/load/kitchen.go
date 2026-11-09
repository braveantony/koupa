package load

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"koupa/internal/wire"
	"koupa/rush-hour/record"
)

// Kitchen behaves like a real clerk: it polls the pending list and marks each
// order of this run ready 1-3 s after first seeing it, including orders the
// customer believed had failed. Orders of other runs are left alone. Each
// order is scheduled once; a failed ready attempt is retried once (same rule
// as customers) with a new attempt id, and is never rescheduled afterwards.
//
// Ready attempt ids are wire.NewAttemptID(RunID+"-k", n, try) where n counts
// orders in the order they were first seen. Every attempt is written to W as
// kind=ready (ready_seq set when the response carried one). Task 19 settles
// the attempts from Attempts() or, equivalently, the kind=ready JSONL lines.
type Kitchen struct {
	Client  *http.Client
	Base    string
	RunID   string
	Timeout time.Duration        // per request; 0 means 5s
	Poll    time.Duration        // pending-list poll interval; 0 means 500ms
	Delay   func() time.Duration // marking delay per order; default uniform 1-3s
	W       *record.Writer

	once     sync.Once
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup // in-flight per-order goroutines

	mu       sync.Mutex
	seen     map[int64]bool
	n        uint64 // orders seen so far
	inflight int
	attempts []string
	err      error
	rng      *rand.Rand
}

const readyRetries = 1

func (k *Kitchen) init() {
	k.once.Do(func() {
		k.stopCh = make(chan struct{})
		k.seen = map[int64]bool{}
		k.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
		if k.Timeout == 0 {
			k.Timeout = 5 * time.Second
		}
		if k.Poll == 0 {
			k.Poll = 500 * time.Millisecond
		}
		if k.Delay == nil {
			k.Delay = func() time.Duration {
				k.mu.Lock()
				defer k.mu.Unlock()
				return time.Second + time.Duration(k.rng.Int63n(int64(2*time.Second)))
			}
		}
	})
}

// Attempts returns the ids of all ready attempts issued so far, in issue order.
func (k *Kitchen) Attempts() []string {
	k.init()
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.attempts...)
}

// Stop stops issuing new ready attempts. Requests already on the wire finish
// (bounded by Timeout); Run returns once they have. Safe to call repeatedly.
func (k *Kitchen) Stop() {
	k.init()
	k.stopOnce.Do(func() { close(k.stopCh) })
}

func (k *Kitchen) stopped() bool {
	select {
	case <-k.stopCh:
		return true
	default:
		return false
	}
}

func (k *Kitchen) setErr(err error) {
	k.mu.Lock()
	if k.err == nil {
		k.err = err
	}
	k.mu.Unlock()
	k.Stop() // an unwritable record file invalidates the run; stop working
}

func (k *Kitchen) firstErr() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.err
}

// Run polls and marks until ctx is done or Stop is called, then waits for
// in-flight requests. It returns the first record-write error, if any.
func (k *Kitchen) Run(ctx context.Context) error {
	k.init()
	for !k.stopped() {
		k.scan(ctx)
		if !k.sleep(ctx, k.Poll) {
			break
		}
	}
	k.wg.Wait()
	return k.firstErr()
}

// Drain keeps marking this run's remaining pending orders until a poll shows
// none unseen and nothing is in flight, or maxWait passes; then it stops
// issuing (as Stop) and waits for requests on the wire.
func (k *Kitchen) Drain(ctx context.Context, maxWait time.Duration) error {
	k.init()
	dctx, cancel := context.WithTimeout(ctx, maxWait)
	defer cancel()
	for !k.stopped() {
		fresh := k.scan(dctx)
		k.mu.Lock()
		idle := k.inflight == 0
		k.mu.Unlock()
		if fresh == 0 && idle {
			break
		}
		if !k.sleep(dctx, k.Poll) {
			break
		}
	}
	k.Stop()
	k.wg.Wait()
	return k.firstErr()
}

// sleep waits d; false means ctx is done or the kitchen was stopped.
func (k *Kitchen) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	case <-k.stopCh:
		return false
	}
}

type pendingOrder struct {
	OrderID   int64  `json:"order_id"`
	ClientRef string `json:"client_ref"`
}

// scan fetches the pending list and schedules every unseen order of this run.
// It returns how many it scheduled; a failed poll schedules none (the next
// poll tries again).
func (k *Kitchen) scan(ctx context.Context) int {
	pctx, cancel := context.WithTimeout(ctx, k.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, k.Base+"/api/orders?status=pending", nil)
	if err != nil {
		return 0
	}
	resp, err := k.Client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	var list []pendingOrder
	if err != nil || resp.StatusCode != http.StatusOK || json.Unmarshal(data, &list) != nil {
		return 0
	}
	prefix := k.RunID + "-"
	fresh := 0
	for _, o := range list {
		if !strings.HasPrefix(o.ClientRef, prefix) {
			continue
		}
		k.mu.Lock()
		if k.seen[o.OrderID] || k.stopped() {
			k.mu.Unlock()
			continue
		}
		k.seen[o.OrderID] = true
		k.n++
		n := k.n
		k.inflight++
		k.wg.Add(1)
		k.mu.Unlock()
		fresh++
		go k.markLater(ctx, o, n)
	}
	return fresh
}

func (k *Kitchen) markLater(ctx context.Context, o pendingOrder, n uint64) {
	defer k.wg.Done()
	defer func() {
		k.mu.Lock()
		k.inflight--
		k.mu.Unlock()
	}()
	scheduled := time.Now().Add(k.Delay())
	if !k.sleep(ctx, time.Until(scheduled)) {
		return // stopped before it was time: never issued
	}
	url := fmt.Sprintf("%s/orders/%d/ready", k.Base, o.OrderID)
	err := sendWithRetry(k.W, readyRetries, func() bool { return !k.stopped() && ctx.Err() == nil }, func(try int) record.Attempt {
		id := wire.NewAttemptID(k.RunID+"-k", n, try)
		k.mu.Lock()
		k.attempts = append(k.attempts, id)
		k.mu.Unlock()
		rec := record.Attempt{Run: k.RunID, Kind: record.KindReady, ClientRef: o.ClientRef, Attempt: id, Try: try, ScheduledAt: scheduled}
		return send(ctx, call{Client: k.Client, Timeout: k.Timeout, Method: http.MethodPost, URL: url}, rec,
			func(r *record.Attempt, body []byte) {
				var out struct {
					OrderID  *int64 `json:"order_id"`
					ReadySeq *int64 `json:"ready_seq"`
				}
				if json.Unmarshal(body, &out) == nil {
					r.OrderID, r.ReadySeq = out.OrderID, out.ReadySeq
				}
			})
	})
	if err != nil {
		k.setErr(err)
	}
}
