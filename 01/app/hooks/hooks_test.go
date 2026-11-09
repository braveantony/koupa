package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"koupa/app/applog"
)

const wait = 5 * time.Second

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) lines() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		if l == "" {
			continue
		}
		m := map[string]any{}
		if err := json.Unmarshal([]byte(l), &m); err == nil {
			out = append(out, m)
		}
	}
	return out
}

func newLog() (*slog.Logger, *syncBuf) {
	buf := &syncBuf{}
	return applog.New(buf, "uid-1", "pod-1", "v1"), buf
}

func post(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	return rec
}

func TestDisabledHooksNoPort(t *testing.T) {
	log, _ := newLog()
	r := New(false, log)
	// Pause returns at once.
	done := make(chan struct{})
	go func() { r.Pause(context.Background(), "request.mid", "a"); close(done) }()
	select {
	case <-done:
	case <-time.After(wait):
		t.Fatal("disabled Pause blocked")
	}
	// Reserve a free port, free it, and check Run does not take it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if err := r.Start(context.Background(), addr); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("disabled hooks opened a port")
	}
}

func TestArmWaitRelease(t *testing.T) {
	log, buf := newLog()
	r := New(true, log)
	r.SetPod("uid-9", "pod-9")
	h := r.Handler()

	// Not armed: no pause.
	r.Pause(context.Background(), "order.before_commit", "x")

	if rec := post(t, h, "/hooks/order.before_commit/arm"); rec.Code != 200 {
		t.Fatalf("arm: %d", rec.Code)
	}
	released := make(chan struct{})
	go func() {
		r.Pause(context.Background(), "order.before_commit", "att-1")
		close(released)
	}()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hooks/order.before_commit/wait?timeout=5s", nil))
	if rec.Code != 200 {
		t.Fatalf("wait: %d %s", rec.Code, rec.Body)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["pod_uid"] != "uid-9" || got["pod_name"] != "pod-9" || got["attempt"] != "att-1" {
		t.Fatalf("wait body = %v", got)
	}
	select {
	case <-released:
		t.Fatal("released before release")
	case <-time.After(50 * time.Millisecond):
	}
	if rec := post(t, h, "/hooks/order.before_commit/release"); rec.Code != 200 {
		t.Fatalf("release: %d", rec.Code)
	}
	select {
	case <-released:
	case <-time.After(wait):
		t.Fatal("Pause not released")
	}
	// Disarmed again: the next request passes.
	r.Pause(context.Background(), "order.before_commit", "att-2")

	found := false
	for _, l := range buf.lines() {
		if l["event"] == applog.EvHookPaused && l["hook"] == "order.before_commit" && l["attempt"] == "att-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("no hook_paused line: %v", buf.lines())
	}
}

func TestPauseEndsWithContext(t *testing.T) {
	log, _ := newLog()
	r := New(true, log)
	post(t, r.Handler(), "/hooks/request.mid/arm")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Pause(ctx, "request.mid", ""); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(wait):
		t.Fatal("Pause ignored ctx")
	}
}

func TestWaitTimeout(t *testing.T) {
	log, _ := newLog()
	r := New(true, log)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hooks/sse.after_hello/wait?timeout=50ms", nil))
	if rec.Code != http.StatusRequestTimeout {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestUnknownHook404(t *testing.T) {
	log, _ := newLog()
	h := New(true, log).Handler()
	for _, p := range []string{"/hooks/nope/arm", "/hooks/nope/release"} {
		if rec := post(t, h, p); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d", p, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hooks/nope/wait", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("wait = %d", rec.Code)
	}
}

func TestStartServesWhenEnabled(t *testing.T) {
	log, _ := newLog()
	r := New(true, log)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx, addr); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post("http://"+addr+"/hooks/nope/arm", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// A second Start on the same address fails: startup must see it.
	if err := r.Start(ctx, addr); err == nil {
		t.Fatal("listen conflict not reported")
	}
}

func wait1(t *testing.T, h http.Handler, timeout string) (int, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hooks/request.mid/wait?timeout="+timeout, nil))
	var m map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func waitPaused(t *testing.T, buf *syncBuf, n int) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		c := 0
		for _, l := range buf.lines() {
			if l["event"] == applog.EvHookPaused {
				c++
			}
		}
		if c >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fewer than %d hook_paused lines", n)
}

// Armed stays armed: every request pauses, wait reports each once, oldest
// first, and release frees all and logs one line each.
func TestStaysArmedUntilRelease(t *testing.T) {
	log, buf := newLog()
	r := New(true, log)
	r.SetPod("u", "p")
	h := r.Handler()
	post(t, h, "/hooks/request.mid/arm")
	var wg sync.WaitGroup
	for _, a := range []string{"a1", "a2"} {
		wg.Add(1)
		go func() { defer wg.Done(); r.Pause(context.Background(), "request.mid", a) }()
		waitPaused(t, buf, map[string]int{"a1": 1, "a2": 2}[a])
	}
	for _, want := range []string{"a1", "a2"} {
		code, m := wait1(t, h, "5s")
		if code != 200 || m["attempt"] != want {
			t.Fatalf("wait = %d %v, want %s", code, m, want)
		}
	}
	if code, _ := wait1(t, h, "50ms"); code != http.StatusRequestTimeout {
		t.Fatalf("third wait = %d, each pause is reported once", code)
	}
	post(t, h, "/hooks/request.mid/release")
	wg.Wait()
	rel := map[any]bool{}
	for _, l := range buf.lines() {
		if l["event"] == applog.EvHookReleased {
			if l["hook"] != "request.mid" || l["pod_uid"] != "uid-1" {
				t.Errorf("release line = %v", l)
			}
			rel[l["attempt"]] = true
		}
	}
	if !rel["a1"] || !rel["a2"] || len(rel) != 2 {
		t.Errorf("release lines = %v", rel)
	}
	// Disarmed by release.
	r.Pause(context.Background(), "request.mid", "a3")
}

// A pause whose ctx ends is never reported afterwards.
func TestCancelledPauseNotReported(t *testing.T) {
	log, buf := newLog()
	r := New(true, log)
	h := r.Handler()
	post(t, h, "/hooks/request.mid/arm")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Pause(ctx, "request.mid", "gone"); close(done) }()
	waitPaused(t, buf, 1)
	cancel()
	<-done
	if code, m := wait1(t, h, "50ms"); code != http.StatusRequestTimeout {
		t.Fatalf("wait named a cancelled pause: %d %v", code, m)
	}
}

// A ctx that has already ended never produces a pause, so nothing is
// reported or logged for it.
func TestPreCancelledNeverReported(t *testing.T) {
	log, buf := newLog()
	r := New(true, log)
	h := r.Handler()
	post(t, h, "/hooks/request.mid/arm")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 50; i++ {
		r.Pause(ctx, "request.mid", "dead")
	}
	if code, m := wait1(t, h, "20ms"); code != http.StatusRequestTimeout {
		t.Fatalf("wait named a pre-cancelled request: %d %v", code, m)
	}
	if n := len(buf.lines()); n != 0 {
		t.Fatalf("logged %d lines for pre-cancelled requests", n)
	}
}

// For each request the pause line comes before its release line, even when
// release runs concurrently with the pause.
func TestPauseLoggedBeforeRelease(t *testing.T) {
	for i := 0; i < 50; i++ {
		log, buf := newLog()
		r := New(true, log)
		h := r.Handler()
		post(t, h, "/hooks/request.mid/arm")
		var wg sync.WaitGroup
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func() { defer wg.Done(); r.Pause(context.Background(), "request.mid", "x") }()
		}
		post(t, h, "/hooks/request.mid/release")
		// Requests that arrive after release pass; release again until
		// every Pause has returned.
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
	loop:
		for {
			select {
			case <-done:
				break loop
			default:
				post(t, h, "/hooks/request.mid/release")
			}
		}
		paused, released := 0, 0
		for _, l := range buf.lines() {
			switch l["event"] {
			case applog.EvHookPaused:
				paused++
			case applog.EvHookReleased:
				released++
				if released > paused {
					t.Fatalf("iteration %d: release logged before its pause", i)
				}
			}
		}
	}
}
