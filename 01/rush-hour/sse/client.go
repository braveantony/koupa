package sse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"sync"
	"time"

	"koupa/internal/wire"
	"koupa/rush-hour/record"
)

// DefaultRetry is the spec's initial reconnection time.
const DefaultRetry = 3000 * time.Millisecond

// Board is one simulated board (browser tab): an EventSource-compatible
// client that records every connection event to W.
//
// Deviation from the spec: an EventSource fails permanently on a non-200
// status or a content type other than text/event-stream. The load tool
// records an "error" event (with the status) and retries after the current
// reconnection time anyway, so a broken server cannot silently end the
// measurement. Network errors (connection refused) are recorded and retried
// the same way, as browsers do.
type Board struct {
	Name  string
	RunID string
	URL   string
	W     *record.BoardWriter
	// Retry is the initial reconnection time; 0 means DefaultRetry. A retry
	// field from the server replaces it.
	Retry time.Duration
	// Client defaults to a client with no overall timeout.
	Client *http.Client

	mu         sync.Mutex
	conns      int
	firstStart *int64
	maxSeq     int64
	received   []int64
}

// FirstStart returns the start from the first hello ever received.
func (b *Board) FirstStart() (int64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.firstStart == nil {
		return 0, false
	}
	return *b.firstStart, true
}

// MaxSeq returns the highest ready_seq received (0 if none).
func (b *Board) MaxSeq() int64 { b.mu.Lock(); defer b.mu.Unlock(); return b.maxSeq }

// Received returns every ready_seq in the order received, duplicates included.
func (b *Board) Received() []int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]int64(nil), b.received...)
}

// Connections returns the number of connection attempts made so far
// (including failed ones); it is the "conn" index of the latest record.
func (b *Board) Connections() int { b.mu.Lock(); defer b.mu.Unlock(); return b.conns }

func (b *Board) emit(e record.BoardEvent) {
	e.Run, e.Board = b.RunID, b.Name
	e.At = time.Now()
	_ = b.W.Write(e) // sticky; the caller checks W.Err()
}

// Run connects, reads until the stream ends, waits the reconnection time and
// reconnects, until ctx is done.
func (b *Board) Run(ctx context.Context) {
	client := b.Client
	if client == nil {
		client = &http.Client{}
	}
	retry := b.Retry
	if retry <= 0 {
		retry = DefaultRetry
	}
	p := &Parser{OnRetry: func(d time.Duration) { retry = d }}
	for ctx.Err() == nil {
		b.mu.Lock()
		b.conns++
		conn := b.conns
		b.mu.Unlock()
		b.once(ctx, client, p, conn)
		if ctx.Err() != nil {
			return
		}
		t := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (b *Board) once(ctx context.Context, client *http.Client, p *Parser, conn int) {
	fail := func(status *int, err error) {
		if ctx.Err() != nil {
			return
		}
		b.emit(record.BoardEvent{Conn: conn, Kind: record.BoardError, Status: status, Err: err.Error()})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL, nil)
	if err != nil {
		fail(nil, err)
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if validHeaderValue(p.LastEventID) {
		req.Header.Set("Last-Event-ID", p.LastEventID)
	}
	resp, err := client.Do(req)
	if err != nil {
		fail(nil, err)
		return
	}
	defer resp.Body.Close()
	st := resp.StatusCode
	if st != http.StatusOK {
		fail(&st, fmt.Errorf("unexpected status %d", st))
		return
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
		fail(&st, fmt.Errorf("unexpected content type %q", resp.Header.Get("Content-Type")))
		return
	}
	var uid *string
	if v := resp.Header.Get(wire.HeaderPodUID); v != "" {
		uid = &v
	}
	b.emit(record.BoardEvent{Conn: conn, Kind: record.BoardConnect, PodUID: uid, Status: &st})

	shutdownSeen := false
	rerr := p.Parse(resp.Body, func(e Event) {
		switch e.Type {
		case "hello":
			ev := record.BoardEvent{Conn: conn, Kind: record.BoardHello, PodUID: uid}
			if n, err := strconv.ParseInt(e.ID, 10, 64); err == nil {
				ev.Start = &n
				b.mu.Lock()
				if b.firstStart == nil {
					v := n
					b.firstStart = &v
				}
				b.mu.Unlock()
			}
			var d struct {
				Version *string `json:"version"`
			}
			if json.Unmarshal([]byte(e.Data), &d) == nil {
				ev.Version = d.Version
			}
			b.emit(ev)
		case "ready":
			ev := record.BoardEvent{Conn: conn, Kind: record.BoardReady, PodUID: uid}
			if n, err := strconv.ParseInt(e.ID, 10, 64); err == nil {
				ev.ReadySeq = &n
				b.mu.Lock()
				b.received = append(b.received, n)
				if n > b.maxSeq {
					b.maxSeq = n
				}
				b.mu.Unlock()
			}
			var d struct {
				Number *string `json:"number"`
			}
			if json.Unmarshal([]byte(e.Data), &d) == nil {
				ev.Number = d.Number
			}
			b.emit(ev)
		case "shutdown":
			shutdownSeen = true
			b.emit(record.BoardEvent{Conn: conn, Kind: record.BoardShutdown, PodUID: uid})
		}
	})
	d := record.BoardEvent{Conn: conn, Kind: record.BoardDisconnect, PodUID: uid, ShutdownSeen: &shutdownSeen}
	if rerr != nil {
		d.Err = rerr.Error()
	} else if ctx.Err() != nil {
		d.Err = errors.Join(ctx.Err()).Error()
	}
	b.emit(d)
}

// validHeaderValue reports whether v is non-empty and can be sent as an HTTP
// header value (no control characters other than tab, no DEL). A last event
// ID with control characters is not sent rather than failing every reconnect.
func validHeaderValue(v string) bool {
	if v == "" {
		return false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}
