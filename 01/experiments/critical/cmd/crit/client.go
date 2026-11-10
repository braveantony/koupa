package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"koupa/internal/wire"
	"koupa/rush-hour/sse"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ------------------------------------------------------------------ HTTP

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Result is one client attempt as the client saw it.
type Result struct {
	Attempt      string          `json:"attempt"`
	SentAt       string          `json:"sent_at"`
	RespondedAt  string          `json:"responded_at"`
	Status       int             `json:"status,omitempty"`
	FullResponse bool            `json:"full_response"`
	FullSuccess  bool            `json:"full_success"`
	Err          string          `json:"err,omitempty"`
	PodUID       string          `json:"pod_uid,omitempty"`
	Body         json.RawMessage `json:"body,omitempty"`
}

// freshClient never reuses a connection, so closing one attempt never
// affects another.
func freshClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
}

func do(ctx context.Context, method, url, attempt string, body any, hdr map[string]string) Result {
	r := Result{Attempt: attempt}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if attempt != "" {
		req.Header.Set(wire.HeaderAttempt, attempt)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	r.SentAt = now()
	resp, err := freshClient().Do(req)
	if err != nil {
		r.RespondedAt, r.Err = now(), err.Error()
		return r
	}
	defer resp.Body.Close()
	r.Status = resp.StatusCode
	r.PodUID = resp.Header.Get(wire.HeaderPodUID)
	data, err := io.ReadAll(resp.Body)
	r.RespondedAt = now()
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.FullResponse = true
	r.FullSuccess = resp.StatusCode/100 == 2
	if json.Valid(data) {
		r.Body = data
	}
	return r
}

type orderBody struct {
	ClientRef string `json:"client_ref"`
	Items     []item `json:"items"`
}

type item struct {
	MenuItemID int `json:"menu_item_id"`
	Quantity   int `json:"quantity"`
}

func order(ref string) orderBody { return orderBody{ClientRef: ref, Items: []item{{1, 1}}} }

// async runs do in the background with its own cancel.
type async struct {
	cancel context.CancelFunc
	done   chan Result
}

func goDo(timeout time.Duration, method, url, attempt string, body any) *async {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	a := &async{cancel: cancel, done: make(chan Result, 1)}
	go func() { a.done <- do(ctx, method, url, attempt, body, nil) }()
	return a
}

func (a *async) result(timeout time.Duration) (Result, error) {
	select {
	case r := <-a.done:
		return r, nil
	case <-time.After(timeout):
		return Result{}, fmt.Errorf("client did not finish within %s", timeout)
	}
}

// placeOrder creates an order with no hook armed and returns its id.
func placeOrder(base, ref, attempt string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := do(ctx, http.MethodPost, base+"/orders", attempt, order(ref), nil)
	var o struct {
		OrderID int64 `json:"order_id"`
	}
	if !r.FullSuccess || json.Unmarshal(r.Body, &o) != nil || o.OrderID == 0 {
		return 0, fmt.Errorf("place order %s: %+v", ref, r)
	}
	return o.OrderID, nil
}

func markReady(base string, id int64, attempt string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := do(ctx, http.MethodPost, fmt.Sprintf("%s/orders/%d/ready", base, id), attempt, nil, nil)
	var o struct {
		ReadySeq int64 `json:"ready_seq"`
	}
	if !r.FullSuccess || json.Unmarshal(r.Body, &o) != nil || o.ReadySeq == 0 {
		return 0, fmt.Errorf("mark ready %d: %+v", id, r)
	}
	step("order %d ready_seq %d", id, o.ReadySeq)
	return o.ReadySeq, nil
}

func auditOrders(base, run string) (map[string][]int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := do(ctx, http.MethodGet, base+"/api/audit?mode=orders&run="+run, "", nil, nil)
	var m map[string][]int64
	if !r.FullSuccess || json.Unmarshal(r.Body, &m) != nil {
		return nil, fmt.Errorf("audit orders: %+v", r)
	}
	return m, nil
}

func common(name string, args []string, extra func(*flag.FlagSet)) (*flag.FlagSet, *string, *string, *string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	base := fs.String("base", "", "base URL")
	run := fs.String("run", "", "run id (client_ref prefix)")
	hk := fs.String("hooks", "", "hook addresses")
	if extra != nil {
		extra(fs)
	}
	err := fs.Parse(args)
	if err == nil && *run == "" {
		err = errors.New("--run required")
	}
	return fs, base, run, hk, err
}

// ------------------------------------------------------------- boards

type board struct {
	mu     sync.Mutex
	events []sse.Event
	cancel context.CancelFunc
	done   chan error
	status int
}

// openBoard connects GET /events (optionally with Last-Event-ID) and
// records every dispatched event until closed.
func openBoard(base, attempt, lastID string) *board {
	ctx, cancel := context.WithCancel(context.Background())
	b := &board{cancel: cancel, done: make(chan error, 1)}
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
		req.Header.Set(wire.HeaderAttempt, attempt)
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		resp, err := freshClient().Do(req)
		if err != nil {
			b.done <- err
			return
		}
		defer resp.Body.Close()
		b.mu.Lock()
		b.status = resp.StatusCode
		b.mu.Unlock()
		var p sse.Parser
		b.done <- p.Parse(resp.Body, func(e sse.Event) {
			b.mu.Lock()
			b.events = append(b.events, e)
			b.mu.Unlock()
		})
	}()
	return b
}

func (b *board) close() { b.cancel(); <-b.done }

func (b *board) snapshot() []sse.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]sse.Event(nil), b.events...)
}

// until waits for an event matching f.
func (b *board) until(f func(sse.Event) bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, e := range b.snapshot() {
			if f(e) {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

type evJSON struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

func (b *board) summary() (helloID string, ready []int64, all []evJSON) {
	for _, e := range b.snapshot() {
		all = append(all, evJSON{e.Type, e.ID})
		switch e.Type {
		case "hello":
			if helloID == "" {
				helloID = e.ID
			}
		case "ready":
			if n, err := strconv.ParseInt(e.ID, 10, 64); err == nil {
				ready = append(ready, n)
			}
		}
	}
	return
}

func isHello(e sse.Event) bool { return e.Type == "hello" }
