package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// namedPause pauses at one point until released and tells the test the
// attempt that arrived.
type namedPause struct {
	name    string
	at      chan string
	release chan struct{}
	once    sync.Once
}

func newNamedPause(t *testing.T, name string) *namedPause {
	p := &namedPause{name: name, at: make(chan string, 1), release: make(chan struct{})}
	t.Cleanup(p.unpause)
	return p
}

func (p *namedPause) unpause() { p.once.Do(func() { close(p.release) }) }

func (p *namedPause) Pause(ctx context.Context, name, attempt string) {
	if name != p.name {
		return
	}
	p.at <- attempt
	select {
	case <-p.release:
	case <-ctx.Done():
	}
}

func countOrdersDB(t *testing.T, e *testEnv, ref string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM orders WHERE client_ref = $1`, ref).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type asyncResp struct {
	code int
	err  error
}

// postOrderAsync sends POST /orders with an attempt header.
func postOrderAsync(e *testEnv, ref, attempt string) <-chan asyncResp {
	out := make(chan asyncResp, 1)
	go func() {
		body := `{"client_ref":"` + ref + `","items":[{"menu_item_id":1,"quantity":1}]}`
		req, _ := http.NewRequest("POST", e.ts.URL+"/orders", strings.NewReader(body))
		req.Header.Set("X-Koupa-Attempt", attempt)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- asyncResp{err: err}
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		out <- asyncResp{code: resp.StatusCode}
	}()
	return out
}

func recvAttempt(t *testing.T, p *namedPause) string {
	t.Helper()
	select {
	case a := <-p.at:
		return a
	case <-time.After(waitTimeout):
		t.Fatal("request never reached the pause point")
		return ""
	}
}

// A12: after order.after_commit the order is in the database but the client
// has no response yet.
func TestAfterCommitPausesAfterCommit(t *testing.T) {
	cfg := testConfig("v1")
	cfg.IdempotencyKey = true
	p := newNamedPause(t, "order.after_commit")
	e := newEnvHooks(t, cfg, nil, p)
	res := postOrderAsync(e, "ac-1", "att-ac")
	if a := recvAttempt(t, p); a != "att-ac" {
		t.Fatalf("attempt = %q", a)
	}
	if n := countOrdersDB(t, e, "ac-1"); n != 1 {
		t.Fatalf("orders while paused after commit = %d, want 1", n)
	}
	select {
	case r := <-res:
		t.Fatalf("client got a response while paused: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	p.unpause()
	r := <-res
	if r.err != nil || r.code != http.StatusCreated {
		t.Fatalf("after release: %+v", r)
	}
}

// A12: request.mid pauses before any transaction, so no order exists.
func TestRequestMidPausesBeforeTransaction(t *testing.T) {
	cfg := testConfig("v1")
	cfg.IdempotencyKey = true
	p := newNamedPause(t, "request.mid")
	e := newEnvHooks(t, cfg, nil, p)
	res := postOrderAsync(e, "mid-1", "att-mid")
	if a := recvAttempt(t, p); a != "att-mid" {
		t.Fatalf("attempt = %q", a)
	}
	if n := countOrdersDB(t, e, "mid-1"); n != 0 {
		t.Fatalf("orders while paused at request.mid = %d", n)
	}
	p.unpause()
	if r := <-res; r.err != nil || r.code != http.StatusCreated {
		t.Fatalf("after release: %+v", r)
	}
}
