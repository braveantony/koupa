// Package broadcast is the per-Pod call-number broadcaster (plan A2). One
// dedicated connection LISTENs on order_ready; a single loop turns every
// trigger (a notification, a fresh LISTEN, a poll tick) into one catch-up
// query "ready_seq > progress ORDER BY ready_seq" and dispatches the rows,
// in order, to every registered subscriber. NOTIFY is only a hint: the
// database rows are the source of truth, so a lost notification is caught
// by the next poll.
package broadcast

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"koupa/app/applog"
	"koupa/app/store"
)

// ReadyEvent is one call-number to dispatch.
type ReadyEvent = store.ReadyEvent

// DropFunc, when it returns true, skips event seq for subscriber subID.
// It exists for the drop-ready test mutation; production passes nil. It is
// called with the broadcaster's lock held and must not call back into it.
type DropFunc func(subID uint64, seq int64) bool

const (
	defaultPageSize = 500
	minBackoff      = 200 * time.Millisecond
	maxBackoff      = 2 * time.Second
	queryTimeout    = 10 * time.Second
	connectTimeout  = 10 * time.Second

	kindNotify   = "notify"
	kindRelisten = "relisten"
	kindPoll     = "poll"
)

// cycle records one trigger's work, for tests.
type cycle struct {
	kind        string
	triggerAt   time.Time
	queryDoneAt time.Time // first page
	dispatchAt  time.Time // start of the last page's dispatch, before any send
	seqs        []int64
}

type subscriber struct {
	ch         chan<- ReadyEvent
	overflow   chan struct{}
	overflowed bool
}

// Broadcaster fans call-numbers out to this Pod's board connections.
type Broadcaster struct {
	pool      *pgxpool.Pool
	listenDSN string
	poll      time.Duration
	log       *slog.Logger
	drop      DropFunc

	ready     chan struct{}
	readyOnce sync.Once
	listening atomic.Bool // a LISTEN connection is active

	// mu guards progress and subs. Dispatch, Register and Unregister all
	// take it, so a subscriber registered at progress P receives exactly
	// the events with ready_seq > P.
	mu       sync.Mutex
	progress int64
	subs     map[uint64]*subscriber
	nextID   uint64

	// Test seams; set before Run.
	pageSize    int
	connectHook func(attempt int)                            // before each connect attempt
	listenHook  func(ctx context.Context, c *pgx.Conn) error // after LISTEN is active
	trace       func(cycle)                                  // after each catch-up
}

// New returns a broadcaster. listenDSN opens the dedicated LISTEN
// connection (not taken from pool); poll is the fallback query interval.
func New(pool *pgxpool.Pool, listenDSN string, poll time.Duration, log *slog.Logger, drop DropFunc) *Broadcaster {
	return &Broadcaster{
		pool:      pool,
		listenDSN: listenDSN,
		poll:      poll,
		log:       log,
		drop:      drop,
		ready:     make(chan struct{}),
		subs:      make(map[uint64]*subscriber),
		pageSize:  defaultPageSize,
	}
}

// Ready is closed once LISTEN is first active and progress is set to the
// then-current MAX(ready_seq).
func (b *Broadcaster) Ready() <-chan struct{} { return b.ready }

// Listening reports whether a LISTEN connection is active right now: false
// before the first LISTEN and from a connection loss until LISTEN is
// re-established. Readiness (/healthz) uses it.
func (b *Broadcaster) Listening() bool { return b.listening.Load() }

// Register waits until the broadcaster is Ready, then adds ch and returns
// its id and the progress at that instant: every event with ready_seq >
// progress will be offered to ch, nothing at or below it. Waiting matters:
// before initialization progress is not yet meaningful, and the jump to
// MAX(ready_seq) dispatches nothing. If ctx ends first it returns ctx's
// error and registers nothing. Sends never block; see Overflowed.
func (b *Broadcaster) Register(ctx context.Context, ch chan<- ReadyEvent) (subID uint64, progress int64, err error) {
	select {
	case <-b.ready:
	case <-ctx.Done():
		return 0, 0, ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	b.subs[b.nextID] = &subscriber{ch: ch, overflow: make(chan struct{})}
	return b.nextID, b.progress, nil
}

// Unregister removes a subscriber. Unknown ids are ignored.
func (b *Broadcaster) Unregister(subID uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, subID)
}

// Overflowed is closed when the subscriber's channel was full at dispatch
// time; it then receives nothing more and should end itself. It returns
// nil (never ready) for an unknown id.
func (b *Broadcaster) Overflowed(subID uint64) <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.subs[subID]; ok {
		return s.overflow
	}
	return nil
}

// Run starts the LISTEN connection and the dispatch loop and returns nil
// once ctx is done. Database errors are logged and retried, never fatal.
func (b *Broadcaster) Run(ctx context.Context) error {
	listenUp := make(chan struct{}, 1)
	notified := make(chan struct{}, 1)
	listenDone := make(chan struct{})
	go func() {
		defer close(listenDone)
		b.listen(ctx, listenUp, notified)
	}()
	defer func() { <-listenDone }()

	ticker := time.NewTicker(b.poll)
	defer ticker.Stop()
	listening, initialized := false, false
	for {
		var kind string
		select {
		case <-ctx.Done():
			return nil
		case <-listenUp:
			kind, listening = kindRelisten, true
		case <-notified:
			kind = kindNotify
		case <-ticker.C:
			kind = kindPoll
		}
		triggerAt := time.Now()
		if !initialized {
			// Progress starts at the max seen by a new statement issued
			// after LISTEN is active: anything committed later notifies.
			if !listening {
				continue
			}
			qctx, cancel := context.WithTimeout(ctx, queryTimeout)
			m, err := store.MaxReadySeq(qctx, b.pool)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					b.log.Warn(applog.EvBroadcastQueryFailed, "trigger", kind, "err", err.Error())
				}
				continue // retried on the next trigger
			}
			b.mu.Lock()
			b.progress = m
			b.mu.Unlock()
			initialized = true
			b.readyOnce.Do(func() { close(b.ready) })
			continue
		}
		b.catchUp(ctx, kind, triggerAt)
	}
}

// catchUp queries everything after progress, page by page, and dispatches.
// Only the loop goroutine writes progress, so reading it here without the
// lock is safe.
func (b *Broadcaster) catchUp(ctx context.Context, kind string, triggerAt time.Time) {
	c := cycle{kind: kind, triggerAt: triggerAt}
	for {
		qctx, cancel := context.WithTimeout(ctx, queryTimeout)
		evs, err := store.ReadyAfter(qctx, b.pool, b.progress, b.pageSize)
		cancel()
		now := time.Now()
		if c.queryDoneAt.IsZero() {
			c.queryDoneAt = now
		}
		if err != nil {
			if ctx.Err() == nil {
				b.log.Warn(applog.EvBroadcastQueryFailed, "trigger", kind, "after", b.progress, "err", err.Error())
			}
			break
		}
		if b.trace != nil {
			c.dispatchAt = time.Now()
			for _, ev := range evs {
				c.seqs = append(c.seqs, ev.ReadySeq)
			}
		}
		b.dispatch(evs)
		if len(evs) < b.pageSize {
			break
		}
	}
	if b.trace != nil {
		b.trace(c)
	}
}

// dispatch offers evs, in order, to every subscriber without blocking.
func (b *Broadcaster) dispatch(evs []ReadyEvent) {
	if len(evs) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ev := range evs {
		for id, s := range b.subs {
			if s.overflowed {
				continue
			}
			if b.drop != nil && b.drop(id, ev.ReadySeq) {
				continue
			}
			select {
			case s.ch <- ev:
			default:
				s.overflowed = true
				close(s.overflow)
				b.log.Warn(applog.EvSubscriberOverflow, "sub_id", id, "ready_seq", ev.ReadySeq)
			}
		}
		b.progress = ev.ReadySeq
	}
}

// listen keeps one LISTEN connection up, reconnecting with backoff.
func (b *Broadcaster) listen(ctx context.Context, listenUp, notified chan<- struct{}) {
	backoff := minBackoff
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return
		}
		if b.connectHook != nil {
			b.connectHook(attempt)
		}
		established, err := b.listenOnce(ctx, listenUp, notified)
		if ctx.Err() != nil {
			return
		}
		if established {
			backoff = minBackoff
		}
		b.log.Warn(applog.EvListenLost, "attempt", attempt, "backoff_ms", backoff.Milliseconds(), "err", err.Error())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// listenOnce connects, LISTENs (autocommit), signals listenUp and forwards
// notifications until the connection fails. established reports whether
// LISTEN became active.
func (b *Broadcaster) listenOnce(ctx context.Context, listenUp, notified chan<- struct{}) (established bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := pgx.Connect(cctx, b.listenDSN)
	if err != nil {
		cancel()
		return false, err
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
		defer closeCancel()
		conn.Close(closeCtx)
	}()
	_, err = conn.Exec(cctx, `LISTEN order_ready`)
	if err == nil && b.listenHook != nil {
		err = b.listenHook(cctx, conn)
	}
	cancel()
	if err != nil {
		return false, err
	}
	b.log.Info(applog.EvListenActive, "pid", conn.PgConn().PID())
	b.listening.Store(true)
	defer b.listening.Store(false)
	signal(listenUp)
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return true, err
		}
		signal(notified)
	}
}

// signal is a coalescing, non-blocking send: one pending trigger is enough
// because every trigger queries everything after progress.
func signal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
