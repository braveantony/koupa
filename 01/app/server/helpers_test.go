package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"koupa/app/applog"
	"koupa/app/broadcast"
	"koupa/app/config"
	"koupa/app/hooks"
	"koupa/internal/testdb"
)

const waitTimeout = 10 * time.Second

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// lines parses every JSON log line written so far.
func (s *syncBuf) lines(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	data := append([]byte(nil), s.b.Bytes()...)
	s.mu.Unlock()
	return parseLines(t, data)
}

func parseLines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad log line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

// find returns the log lines with the given event whose fields match kv.
func find(lines []map[string]any, event string, kv ...any) []map[string]any {
	var out []map[string]any
next:
	for _, l := range lines {
		if l["event"] != event {
			continue
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if l[kv[i].(string)] != kv[i+1] {
				continue next
			}
		}
		out = append(out, l)
	}
	return out
}

// awaitLog polls buf until a line matching event and kv appears.
func awaitLog(t *testing.T, buf *syncBuf, event string, kv ...any) map[string]any {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if m := find(buf.lines(t), event, kv...); len(m) > 0 {
			return m[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s %v in log:\n%s", event, kv, buf.b.String())
	return nil
}

// listenDSN is a DSN for the test database as role koupa.
func listenDSN(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	cc := pool.Config().ConnConfig
	u, err := url.Parse(os.Getenv("KOUPA_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(cc.User, cc.Password)
	u.Path = "/" + cc.Database
	return u.String()
}

func testConfig(version string) config.Config {
	return config.Config{
		PodUID: "uid-test", PodName: "pod-test", Version: version,
		ShutdownTimeout: 5 * time.Second, SSERetry: time.Second,
		LockTimeout: 5 * time.Second, DBTimeout: 60 * time.Second,
		BroadcastPoll: 100 * time.Millisecond, WriteDeadline: 2 * time.Second,
	}
}

// runBroadcaster runs b until the test ends.
func runBroadcaster(t *testing.T, b *broadcast.Broadcaster) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = b.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(waitTimeout):
			t.Error("broadcaster did not stop")
		}
	})
}

type testEnv struct {
	srv  *Server
	ts   *httptest.Server
	pool *pgxpool.Pool
	log  *syncBuf
	cfg  config.Config
}

// newEnv starts an in-process server on a fresh database with a running
// broadcaster, marked started (as Run does after listening).
func newEnv(t *testing.T, cfg config.Config, pool *pgxpool.Pool) *testEnv {
	t.Helper()
	if pool == nil {
		pool = testdb.New(t)
	}
	buf := &syncBuf{}
	log := applog.New(buf, cfg.PodUID, cfg.PodName, cfg.Version)
	b := broadcast.New(pool, listenDSN(t, pool), cfg.BroadcastPoll, log, nil)
	runBroadcaster(t, b)
	select {
	case <-b.Ready():
	case <-time.After(waitTimeout):
		t.Fatal("broadcaster not ready")
	}
	s := New(cfg, Deps{Pool: pool, Broadcaster: b, Log: log, Hooks: hooks.Noop{}})
	s.started.Store(true)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{srv: s, ts: ts, pool: pool, log: buf, cfg: cfg}
}

func quietLog() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// do sends a request and decodes a JSON body into out (if non-nil).
func (e *testEnv) do(t *testing.T, method, path, attempt, body string, out any) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if attempt != "" {
		req.Header.Set("X-Koupa-Attempt", attempt)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	return resp
}

func stringReader(s string) io.Reader { return strings.NewReader(s) }

func newPlainServer(t *testing.T, s *Server) string {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func get(t *testing.T, u string) int {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}
