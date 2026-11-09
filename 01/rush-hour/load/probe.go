package load

import (
	"context"
	"net/http"
	"sync"
	"time"

	"koupa/internal/wire"
	"koupa/rush-hour/record"
)

// Prober sends calibration traffic: GET /api/whoami at a fixed Rate (default
// 50/s) on a new connection per request, never waiting for a response before
// the next slot. Each probe is a kind=probe record whose attempt id is
// wire.NewAttemptID(RunID+"-p", seq, 1) with seq counting from 1; pod_uid is
// empty (null) when no response arrived.
type Prober struct {
	// Any client works: every probe is sent with Connection: close, so no
	// connection is reused. Nil means a plain client with keep-alives off.
	Client  *http.Client
	Base    string
	RunID   string
	Rate    float64       // probes per second; 0 means 50
	Timeout time.Duration // per probe; 0 means 2s
	W       *record.Writer
}

// Run sends probes until ctx is done, then waits for the ones still in
// flight. It returns the first record-write error, if any.
func (p *Prober) Run(ctx context.Context) error {
	rate, timeout, client := p.Rate, p.Timeout, p.Client
	if rate == 0 {
		rate = 50
	}
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	if client == nil {
		client = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	}
	sched := &Scheduler{Rate: rate, Start: time.Now()}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var werr error
	for seq := uint64(1); ; seq++ {
		at := sched.Next()
		if !Wait(ctx.Done(), at) || ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(seq uint64, at time.Time) {
			defer wg.Done()
			rec := record.Attempt{Run: p.RunID, Kind: record.KindProbe, Attempt: wire.NewAttemptID(p.RunID+"-p", seq, 1), Try: 1, ScheduledAt: at}
			// Not tied to ctx: a probe already due completes (bounded by
			// timeout) so the final slots are not recorded as cancelled.
			rec = send(context.Background(), call{Client: client, Timeout: timeout, Method: http.MethodGet, URL: p.Base + "/api/whoami", Close: true}, rec, nil)
			if err := p.W.Write(rec); err != nil {
				mu.Lock()
				if werr == nil {
					werr = err
				}
				mu.Unlock()
			}
		}(seq, at)
	}
	wg.Wait()
	return werr
}
