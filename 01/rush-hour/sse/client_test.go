package sse

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"koupa/internal/wire"
	"koupa/rush-hour/record"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) events(t *testing.T) []record.BoardEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []record.BoardEvent
	for _, l := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		if l == "" {
			continue
		}
		var e record.BoardEvent
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// script serves one scripted body per connection (last one repeats as a
// held-open stream) and records request headers.
type script struct {
	mu      sync.Mutex
	bodies  []string
	status  []int
	ctype   string
	headers []http.Header
}

func (s *script) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	i := len(s.headers)
	s.headers = append(s.headers, r.Header.Clone())
	s.mu.Unlock()
	if i >= len(s.bodies) {
		i = len(s.bodies) - 1
	}
	ct := s.ctype
	if ct == "" {
		ct = "text/event-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set(wire.HeaderPodUID, "pod-"+string(rune('a'+i)))
	st := 200
	if i < len(s.status) && s.status[i] != 0 {
		st = s.status[i]
	}
	w.WriteHeader(st)
	_, _ = w.Write([]byte(s.bodies[i]))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if i == len(s.bodies)-1 && len(s.bodies) > 0 && strings.HasSuffix(s.bodies[i], "HOLD") {
		<-r.Context().Done()
	}
}

func runBoard(t *testing.T, h http.Handler, retry time.Duration, until func(*Board, *syncBuf) bool) (*Board, *syncBuf) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	buf := &syncBuf{}
	b := &Board{Name: "b1", RunID: "r1", URL: srv.URL, W: record.NewBoardWriter(buf), Retry: retry}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for !until(b, buf) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	return b, buf
}

func kinds(es []record.BoardEvent) []string {
	var k []string
	for _, e := range es {
		k = append(k, e.Kind)
	}
	return k
}

func TestReconnectSendsLastEventID(t *testing.T) {
	s := &script{bodies: []string{
		"id: 5\nevent: hello\ndata: {\"version\":\"v2\"}\n\nid: 7\nevent: ready\ndata: {\"number\":\"0058\"}\n\n",
		"id: 7\nevent: ready\ndata: {\"number\":\"0058\"}\n\nHOLD",
	}}
	b, _ := runBoard(t, s, 10*time.Millisecond, func(b *Board, _ *syncBuf) bool { return len(b.Received()) >= 2 })
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.headers) < 2 {
		t.Fatalf("want 2 connections, got %d", len(s.headers))
	}
	if v := s.headers[0].Get("Last-Event-ID"); v != "" {
		t.Errorf("first connection sent Last-Event-ID %q", v)
	}
	if v := s.headers[1].Get("Last-Event-ID"); v != "7" {
		t.Errorf("second connection Last-Event-ID=%q want 7", v)
	}
	if a := s.headers[0].Get("Accept"); a != "text/event-stream" {
		t.Errorf("Accept=%q", a)
	}
	if c := s.headers[0].Get("Cache-Control"); c != "no-cache" {
		t.Errorf("Cache-Control=%q", c)
	}
	if st, ok := b.FirstStart(); !ok || st != 5 {
		t.Errorf("FirstStart=%d,%v", st, ok)
	}
	if got := b.Received(); len(got) != 2 || got[0] != 7 || got[1] != 7 {
		t.Errorf("Received=%v (duplicates must be kept)", got)
	}
	if b.MaxSeq() != 7 || b.Connections() < 2 {
		t.Errorf("MaxSeq=%d Connections=%d", b.MaxSeq(), b.Connections())
	}
}

func TestHelloIDCountsForLastEventID(t *testing.T) {
	s := &script{bodies: []string{"id: 3\nevent: hello\ndata: {\"version\":\"v1\"}\n\n", "HOLD"}}
	runBoard(t, s, 10*time.Millisecond, func(*Board, *syncBuf) bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.headers) >= 2 })
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.headers[1].Get("Last-Event-ID"); v != "3" {
		t.Fatalf("Last-Event-ID=%q want 3", v)
	}
}

func TestShutdownThenDisconnectRecorded(t *testing.T) {
	s := &script{bodies: []string{
		"id: 5\nevent: hello\ndata: {\"version\":\"v2\",\"pod_name\":\"koupa-abc\",\"pod_uid\":\"uid-1\"}\n\nid: 6\nevent: ready\ndata: {\"number\":\"0058\"}\n\n: hb\n\nretry: 20\n\nevent: shutdown\ndata:\n\n",
		"id: 6\nevent: hello\ndata: {\"version\":\"v2\"}\n\n",
		"HOLD",
	}}
	_, buf := runBoard(t, s, 10*time.Millisecond, func(_ *Board, b *syncBuf) bool { return len(b.events(t)) >= 8 })
	es := buf.events(t)
	want := []string{"connect", "hello", "ready", "shutdown", "disconnect", "connect", "hello", "disconnect"}
	if got := kinds(es); len(got) < len(want) || strings.Join(got[:len(want)], ",") != strings.Join(want, ",") {
		t.Fatalf("kinds=%v", got)
	}
	if es[0].PodUID == nil || *es[0].PodUID != "pod-a" || es[0].Status == nil || *es[0].Status != 200 || es[0].Conn != 1 {
		t.Errorf("connect=%+v", es[0])
	}
	if es[1].Start == nil || *es[1].Start != 5 || es[1].Version == nil || *es[1].Version != "v2" {
		t.Errorf("hello=%+v", es[1])
	}
	if es[2].ReadySeq == nil || *es[2].ReadySeq != 6 || es[2].Number == nil || *es[2].Number != "0058" {
		t.Errorf("ready=%+v", es[2])
	}
	if es[4].ShutdownSeen == nil || !*es[4].ShutdownSeen {
		t.Errorf("first disconnect should have shutdown_seen=true: %+v", es[4])
	}
	if es[7].ShutdownSeen == nil || *es[7].ShutdownSeen || es[7].Conn != 2 {
		t.Errorf("second disconnect should have shutdown_seen=false: %+v", es[7])
	}
	if es[0].Run != "r1" || es[0].Board != "b1" || es[0].At.IsZero() {
		t.Errorf("run/board/at: %+v", es[0])
	}
}

func TestRetryFieldShortensReconnectDelay(t *testing.T) {
	s := &script{bodies: []string{"retry: 20\n\n", "HOLD"}}
	start := time.Now()
	runBoard(t, s, 10*time.Second, func(*Board, *syncBuf) bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.headers) >= 2 })
	if time.Since(start) > 3*time.Second {
		t.Fatalf("retry: 20 was not honored, took %v", time.Since(start))
	}
}

func TestNon200IsRecordedWithStatusAndRetried(t *testing.T) {
	s := &script{bodies: []string{"bad", "HOLD"}, status: []int{400, 200}}
	_, buf := runBoard(t, s, 10*time.Millisecond, func(_ *Board, b *syncBuf) bool { return len(b.events(t)) >= 2 })
	es := buf.events(t)
	if es[0].Kind != "error" || es[0].Status == nil || *es[0].Status != 400 || es[0].Err == "" {
		t.Fatalf("first=%+v", es[0])
	}
	if es[1].Kind != "connect" {
		t.Fatalf("not retried: %v", kinds(es))
	}
}

func TestWrongContentTypeIsError(t *testing.T) {
	s := &script{bodies: []string{"data: x\n\n", "HOLD"}, ctype: "text/plain"}
	_, buf := runBoard(t, s, 10*time.Millisecond, func(_ *Board, b *syncBuf) bool { return len(b.events(t)) >= 1 })
	es := buf.events(t)
	if es[0].Kind != "error" || es[0].Status == nil || *es[0].Status != 200 {
		t.Fatalf("got %+v", es[0])
	}
}

func TestConnectionRefusedIsError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	buf := &syncBuf{}
	b := &Board{Name: "b", RunID: "r", URL: url, W: record.NewBoardWriter(buf), Retry: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	b.Run(ctx)
	es := buf.events(t)
	if len(es) != 1 || es[0].Kind != "error" || es[0].Status != nil || es[0].Err == "" {
		t.Fatalf("got %+v", es)
	}
}

func TestNewStreamWithoutIDClearsLastEventID(t *testing.T) {
	s := &script{bodies: []string{
		"id: 4\nevent: ready\ndata: {}\n\n",
		"data: x\n\n",
		"HOLD",
	}}
	runBoard(t, s, 10*time.Millisecond, func(*Board, *syncBuf) bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.headers) >= 3 })
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.headers[1].Get("Last-Event-ID"); v != "4" {
		t.Errorf("second: %q want 4", v)
	}
	if _, ok := s.headers[2]["Last-Event-Id"]; ok {
		t.Errorf("third connection must not send Last-Event-ID, got %q", s.headers[2].Get("Last-Event-ID"))
	}
}

func TestMalformedIDStillReconnects(t *testing.T) {
	for name, id := range map[string]string{"invalid utf8": "7\xff", "control char": "7\x01"} {
		s := &script{bodies: []string{"id: " + id + "\ndata: x\n\n", "HOLD"}}
		runBoard(t, s, 10*time.Millisecond, func(*Board, *syncBuf) bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.headers) >= 2 })
		s.mu.Lock()
		if len(s.headers) < 2 {
			t.Fatalf("%s: no reconnect", name)
		}
		if name == "invalid utf8" && s.headers[1].Get("Last-Event-ID") != "7\u00ef\u00bf\u00bd" && s.headers[1].Get("Last-Event-ID") != "7\ufffd" {
			t.Errorf("%s: header=%q", name, s.headers[1].Get("Last-Event-ID"))
		}
		s.mu.Unlock()
	}
}
