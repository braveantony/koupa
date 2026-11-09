package winddown

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"koupa/internal/wire"
)

// fakeShop is a minimal in-memory Koupa: orders, pending list, ready,
// /events and /api/audit.
type fakeShop struct {
	mu      sync.Mutex
	orders  map[string][]int64 // client_ref -> ids
	refOf   map[int64]string
	seq     map[int64]int64
	att     map[int64]string
	nextID  int64
	nextSeq int64
	subs    []chan int64
	failRef string // POST /orders for this ref answers 503 on its first try
}

func newFakeShop() *fakeShop {
	return &fakeShop{orders: map[string][]int64{}, refOf: map[int64]string{}, seq: map[int64]int64{}, att: map[int64]string{}}
}

func (s *fakeShop) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(wire.HeaderPodUID, "uid-fake")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/events":
		w.Header().Set("Content-Type", "text/event-stream")
		ch := make(chan int64, 1024)
		s.mu.Lock()
		s.subs = append(s.subs, ch)
		start := s.nextSeq
		s.mu.Unlock()
		fmt.Fprintf(w, "event: hello\nid: %d\ndata: {\"version\":\"v1\"}\n\n", start)
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case n := <-ch:
				fmt.Fprintf(w, "event: ready\nid: %d\ndata: {\"number\":\"A%03d\"}\n\n", n, n)
				w.(http.Flusher).Flush()
			}
		}
	case r.Method == http.MethodPost && r.URL.Path == "/orders":
		var body struct {
			ClientRef string `json:"client_ref"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.ClientRef == s.failRef && strings.HasSuffix(r.Header.Get(wire.HeaderAttempt), "-t1") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		s.mu.Lock()
		ids := s.orders[body.ClientRef]
		if len(ids) == 0 {
			s.nextID++
			ids = []int64{s.nextID}
			s.orders[body.ClientRef] = ids
			s.refOf[s.nextID] = body.ClientRef
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"order_id":%d}`, ids[0])
	case r.Method == http.MethodGet && r.URL.Path == "/api/orders":
		s.mu.Lock()
		type p struct {
			OrderID   int64  `json:"order_id"`
			ClientRef string `json:"client_ref"`
		}
		list := []p{}
		for id, ref := range s.refOf {
			if _, done := s.seq[id]; !done {
				list = append(list, p{id, ref})
			}
		}
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/orders/"):
		id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/orders/"), "/ready"), 10, 64)
		s.mu.Lock()
		n, done := s.seq[id]
		if !done {
			s.nextSeq++
			n = s.nextSeq
			s.seq[id] = n
			s.att[id] = r.Header.Get(wire.HeaderAttempt)
			for _, c := range s.subs {
				c <- n
			}
		}
		s.mu.Unlock()
		fmt.Fprintf(w, `{"order_id":%d,"ready_seq":%d}`, id, n)
	case r.URL.Path == "/api/audit":
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Query().Get("mode") {
		case "cursor":
			var c int64
			for _, n := range s.seq {
				c = max(c, n)
			}
			fmt.Fprintf(w, `{"cursor":%d}`, c)
		case "orders":
			_ = json.NewEncoder(w).Encode(s.orders)
		case "ready":
			var rows []AuditReady
			for id, n := range s.seq {
				a := s.att[id]
				rows = append(rows, AuditReady{OrderID: id, ReadySeq: n, ReadyAttempt: &a})
			}
			if rows == nil {
				rows = []AuditReady{}
			}
			_ = json.NewEncoder(w).Encode(rows)
		}
	default:
		http.NotFound(w, r)
	}
}

func TestRunAgainstFakeShop(t *testing.T) {
	if testing.Short() {
		t.Skip("sends for a few seconds")
	}
	shop := newFakeShop()
	shop.failRef = "rt-c2"
	srv := httptest.NewServer(shop)
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "run")
	var stdout bytes.Buffer
	l := DefaultLimits(time.Second)
	l.Poll = 100 * time.Millisecond
	l.Kitchen = 5 * time.Second
	l.Server = 500 * time.Millisecond
	sc, err := Run(context.Background(), Config{
		Base: srv.URL, Run: "rt", Duration: 1500 * time.Millisecond, Rate: 4, Boards: 2, Out: out,
		TxTimeout: time.Second, NoIngress: true, Retries: 1, Timeout: 2 * time.Second, Limits: l, Stdout: &stdout,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(stdout.String(), "\n")
	if !strings.HasPrefix(first, "SENDING_STARTED ") {
		t.Fatalf("first stdout line %q", first)
	}
	if _, err := strconv.ParseInt(strings.TrimPrefix(first, "SENDING_STARTED "), 10, 64); err != nil {
		t.Fatalf("SENDING_STARTED value: %v", err)
	}
	for _, f := range []string{"attempts.jsonl", "boards.jsonl", "scorecard.txt", "scorecard.json"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Error(err)
		}
	}
	if sc.Invalid || sc.ExitCode() != 0 {
		t.Fatalf("invalid: %v\n%s", sc.InvalidReasons, sc.Text(true))
	}
	// The 503 has no App log here, so it stays unsettled; everything else
	// is decided.
	if sc.Items.Orders.Confirmed != 6 || sc.Items.FirstTryFailed != (Item{1, 0}) || sc.Items.FailedAfterRetry != (Item{0, 0}) {
		t.Errorf("items %+v\n%s", sc.Items, sc.Text(true))
	}
	for name, m := range sc.Items.Missing {
		if m != (Item{}) {
			t.Errorf("%s missing %+v", name, m)
		}
	}
	if len(sc.Diag.Unsettled) != 1 || sc.Diag.Unsettled[0].Attempt != "rt-a2-t1" {
		t.Errorf("unsettled %+v", sc.Diag.Unsettled)
	}
	if len(sc.Items.Missing) != 2 || sc.Diag.PendingOrders == nil || *sc.Diag.PendingOrders != 0 {
		t.Errorf("boards %v pending %v", sc.Items.Missing, sc.Diag.PendingOrders)
	}
	// Running again into the same directory must not mix evidence.
	if _, err := Run(context.Background(), Config{Base: srv.URL, Run: "rt", Out: out, Boards: 1, Rate: 1, Duration: time.Second, TxTimeout: time.Second}); err == nil {
		t.Error("second run into the same directory accepted")
	}
}

func TestRunInvalidWhenNoHello(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "run")
	var stdout bytes.Buffer
	l := DefaultLimits(time.Second)
	l.Hello = 300 * time.Millisecond
	sc, err := Run(context.Background(), Config{
		Base: srv.URL, Run: "rn", Duration: time.Second, Rate: 1, Boards: 1, Out: out,
		TxTimeout: time.Second, NoIngress: true, Limits: l, Stdout: &stdout,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "SENDING_STARTED") {
		t.Error("sending started without hello")
	}
	if sc.ExitCode() != 2 || !strings.HasPrefix(sc.Text(false), "本次量測無效") {
		t.Errorf("exit %d\n%s", sc.ExitCode(), sc.Text(false))
	}
}

func TestRunSendsUntilD(t *testing.T) {
	if testing.Short() {
		t.Skip("sends for a few seconds")
	}
	srv := httptest.NewServer(newFakeShop())
	defer srv.Close()
	l := DefaultLimits(time.Second)
	l.Poll, l.Server, l.Kitchen = 50*time.Millisecond, 200*time.Millisecond, 4*time.Second
	const d = 2500 * time.Millisecond // rate 1: the last slot is at 2s
	sc, err := Run(context.Background(), Config{
		Base: srv.URL, Run: "rd", Duration: d, Rate: 1, Boards: 1, Out: filepath.Join(t.TempDir(), "run"),
		TxTimeout: time.Second, NoIngress: true, Limits: l,
	})
	if err != nil {
		t.Fatal(err)
	}
	var start, stop time.Time
	for _, e := range sc.Facts.Timeline {
		switch e.What {
		case "開始送單":
			start = e.At
		case "停止送新單":
			stop = e.At
		}
	}
	if start.IsZero() || stop.IsZero() || stop.Sub(start) < d {
		t.Errorf("sending lasted %v, want at least %v", stop.Sub(start), d)
	}
	if sc.Items.Orders.Confirmed != 3 {
		t.Errorf("orders %+v", sc.Items.Orders)
	}
}
