package testproxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// recorder is an upstream that answers "ok <path> <X-Test>" and records
// every request it received, in order.
type recorder struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []string // "<method> <path> <X-Test> <body>"
	hit chan string
}

func newRecorder(t *testing.T) *recorder {
	r := &recorder{hit: make(chan string, 16)}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		s := fmt.Sprintf("%s %s %s %s", req.Method, req.URL.Path, req.Header.Get("X-Test"), b)
		r.mu.Lock()
		r.got = append(r.got, s)
		r.mu.Unlock()
		r.hit <- s
		w.Header().Set("X-Upstream", "1")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "ok %s %s", req.URL.Path, req.Header.Get("X-Test"))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *recorder) addr() string { return strings.TrimPrefix(r.srv.URL, "http://") }

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// waitHit returns the next request the upstream received, or fails.
func (r *recorder) waitHit(t *testing.T, d time.Duration) string {
	t.Helper()
	select {
	case s := <-r.hit:
		return s
	case <-time.After(d):
		t.Fatalf("upstream got no request within %v", d)
		return ""
	}
}

// noHit asserts the upstream receives nothing for d.
func (r *recorder) noHit(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case s := <-r.hit:
		t.Fatalf("upstream got %q, want nothing yet", s)
	case <-time.After(d):
	}
}

type result struct {
	status int
	body   string
	err    error
}

// send posts body to the proxy in the background with header X-Test: tag.
func send(p *HTTP, path, tag, body string) <-chan result {
	ch := make(chan result, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://"+p.Addr()+path, strings.NewReader(body))
		req.Header.Set("X-Test", tag)
		c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second}
		resp, err := c.Do(req)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		ch <- result{status: resp.StatusCode, body: string(b), err: err}
	}()
	return ch
}

func waitResult(t *testing.T, ch <-chan result, d time.Duration) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(d):
		t.Fatalf("client got no result within %v", d)
		return result{}
	}
}

func notYet(t *testing.T, ch <-chan result, d time.Duration) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("client already got %+v, want it still waiting", r)
	case <-time.After(d):
	}
}

func tagIs(tag string) func(*http.Request) bool {
	return func(r *http.Request) bool { return r.Header.Get("X-Test") == tag }
}

func TestHoldAndRelease(t *testing.T) {
	up := newRecorder(t)
	p := NewHTTP(up.addr())
	defer p.Close()

	release := p.HoldNext(tagIs("held"))
	held := send(p, "/orders", "held", "b1")
	up.noHit(t, 300*time.Millisecond)
	notYet(t, held, 0)

	// Other requests pass while one is held.
	r := waitResult(t, send(p, "/other", "free", "b2"), 5*time.Second)
	if r.err != nil || r.status != http.StatusCreated || r.body != "ok /other free" {
		t.Fatalf("free request: %+v", r)
	}
	if s := up.waitHit(t, time.Second); s != "POST /other free b2" {
		t.Fatalf("upstream got %q", s)
	}
	notYet(t, held, 100*time.Millisecond)

	release()
	release() // idempotent
	if s := up.waitHit(t, 5*time.Second); s != "POST /orders held b1" {
		t.Fatalf("upstream got %q, want the held request with its body", s)
	}
	r = waitResult(t, held, 5*time.Second)
	if r.err != nil || r.status != http.StatusCreated || r.body != "ok /orders held" {
		t.Fatalf("held request: %+v", r)
	}

	// The rule was used up: the next matching request passes at once.
	r = waitResult(t, send(p, "/orders", "held", "b3"), 5*time.Second)
	if r.err != nil || r.status != http.StatusCreated {
		t.Fatalf("second matching request: %+v", r)
	}
	if n := up.count(); n != 3 {
		t.Fatalf("upstream count = %d, want 3", n)
	}
}

func TestHoldNextForwardsAfterClientLeft(t *testing.T) {
	up := newRecorder(t)
	p := NewHTTP(up.addr())
	defer p.Close()

	release := p.HoldNext(tagIs("late"))
	req, _ := http.NewRequest(http.MethodPost, "http://"+p.Addr()+"/orders", strings.NewReader("x"))
	req.Header.Set("X-Test", "late")
	c := &http.Client{Timeout: 300 * time.Millisecond}
	if _, err := c.Do(req); err == nil {
		t.Fatal("want a client timeout while the request is held")
	}
	up.noHit(t, 200*time.Millisecond)
	release()
	if s := up.waitHit(t, 5*time.Second); s != "POST /orders late x" {
		t.Fatalf("upstream got %q: a held request must arrive late", s)
	}
}

func TestDropNextResetsClientAndForwardsOnRelease(t *testing.T) {
	up := newRecorder(t)
	p := NewHTTP(up.addr())
	defer p.Close()

	before := make(chan struct{})
	release := p.DropNext(tagIs("drop"), func() { close(before) })
	r := waitResult(t, send(p, "/orders", "drop", "d1"), 5*time.Second)
	if r.err == nil {
		t.Fatalf("client got %+v, want a reset", r)
	}
	if !errors.Is(r.err, syscall.ECONNRESET) && !errors.Is(r.err, io.EOF) && !strings.Contains(r.err.Error(), "reset") {
		t.Errorf("client error = %v, want a connection reset", r.err)
	}
	select {
	case <-before:
	default:
		t.Fatal("before was not called before the reset")
	}
	up.noHit(t, 300*time.Millisecond)
	release()
	if s := up.waitHit(t, 5*time.Second); s != "POST /orders drop d1" {
		t.Fatalf("upstream got %q", s)
	}
}

func TestHoldResponseNext(t *testing.T) {
	up := newRecorder(t)
	p := NewHTTP(up.addr())
	defer p.Close()

	release := p.HoldResponseNext(tagIs("slow"))
	ch := send(p, "/orders", "slow", "s1")
	if s := up.waitHit(t, 5*time.Second); s != "POST /orders slow s1" {
		t.Fatalf("upstream got %q", s)
	}
	notYet(t, ch, 300*time.Millisecond)
	release()
	r := waitResult(t, ch, 5*time.Second)
	if r.err != nil || r.status != http.StatusCreated || r.body != "ok /orders slow" {
		t.Fatalf("held response: %+v", r)
	}
}

func TestCutResponseNext(t *testing.T) {
	up := newRecorder(t)
	p := NewHTTP(up.addr())
	defer p.Close()

	p.CutResponseNext(tagIs("cut"))
	r := waitResult(t, send(p, "/orders", "cut", "c1"), 5*time.Second)
	if r.err == nil {
		t.Fatalf("client got %+v, want the connection reset instead of the response", r)
	}
	if s := up.waitHit(t, time.Second); s != "POST /orders cut c1" {
		t.Fatalf("upstream got %q: the request must reach upstream", s)
	}
}

func TestRefuse(t *testing.T) {
	up := newRecorder(t)
	p := NewHTTP(up.addr())
	defer p.Close()

	resume := p.Refuse()
	_, err := net.DialTimeout("tcp", p.Addr(), time.Second)
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dial while refusing: %v, want connection refused", err)
	}
	resume()
	resume() // idempotent
	r := waitResult(t, send(p, "/x", "after", ""), 5*time.Second)
	if r.err != nil || r.status != http.StatusCreated {
		t.Fatalf("after resume: %+v", r)
	}
}

func TestHTTPCloseWithHeldRequest(t *testing.T) {
	up := newRecorder(t)
	p := NewHTTP(up.addr())
	p.HoldNext(tagIs("held"))
	p.DropNext(tagIs("drop"), nil)
	held := send(p, "/a", "held", "")
	dropped := send(p, "/b", "drop", "")
	waitResult(t, dropped, 5*time.Second)
	time.Sleep(100 * time.Millisecond)

	done := make(chan struct{})
	go func() { p.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hangs on held requests")
	}
	waitResult(t, held, 5*time.Second)
	if n := up.count(); n != 0 {
		t.Fatalf("upstream count = %d: Close must not forward held requests", n)
	}
}
