package load

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"koupa/internal/wire"
	"koupa/rush-hour/record"
)

// fakeShop is a pending list plus a ready endpoint.
type fakeShop struct {
	mu      sync.Mutex
	pending []int64          // order ids still pending, in order
	refs    map[int64]string // order id -> client_ref
	posts   map[int64]int    // order id -> ready POSTs seen
	failFor map[int64]int    // order id -> number of leading POSTs to fail with 503
	seq     int64
	attIDs  []string
	getNull int // leading GETs that answer 200 with JSON null
	getFail int // leading GETs of the pending list that fail with 503
	gets    int
	queries []string
}

func newShop() *fakeShop {
	return &fakeShop{refs: map[int64]string{}, posts: map[int64]int{}, failFor: map[int64]int{}}
}

func (f *fakeShop) add(id int64, ref string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append(f.pending, id)
	f.refs[id] = ref
}

func (f *fakeShop) totalPosts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.posts {
		n += c
	}
	return n
}

func (f *fakeShop) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/orders", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.gets++
		f.queries = append(f.queries, r.URL.RawQuery)
		if f.gets <= f.getFail {
			http.Error(w, "down", 503)
			return
		}
		if f.gets <= f.getNull {
			io.WriteString(w, "null")
			return
		}
		var parts []string
		for _, id := range f.pending {
			parts = append(parts, fmt.Sprintf(`{"order_id":%d,"number":"%04d","total":100,"client_ref":%q,"items":[{"name":"x","quantity":1}]}`, id, id, f.refs[id]))
		}
		io.WriteString(w, "["+strings.Join(parts, ",")+"]")
	})
	mux.HandleFunc("POST /orders/{id}/ready", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.posts[id]++
		f.attIDs = append(f.attIDs, r.Header.Get(wire.HeaderAttempt))
		w.Header().Set(wire.HeaderPodUID, "pod-k")
		if f.posts[id] <= f.failFor[id] {
			http.Error(w, "boom", 503)
			return
		}
		f.seq++
		for i, p := range f.pending {
			if p == id {
				f.pending = append(f.pending[:i], f.pending[i+1:]...)
				break
			}
		}
		fmt.Fprintf(w, `{"order_id":%d,"ready_seq":%d,"number":"%04d","newly_assigned":true}`, id, f.seq, id)
	})
	return mux
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for !cond() {
		if time.Now().After(end) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newKitchen(srv *httptest.Server, buf *bytes.Buffer) *Kitchen {
	return &Kitchen{Client: srv.Client(), Base: srv.URL, RunID: "runK", Timeout: time.Second,
		Poll: 10 * time.Millisecond, Delay: func() time.Duration { return 5 * time.Millisecond },
		W: record.NewWriter(buf)}
}

func TestKitchenMarksEachPendingOnce(t *testing.T) {
	shop := newShop()
	shop.add(1, "runK-c1")
	shop.add(2, "runK-c2")
	shop.add(3, "otherRun-c1") // another run's order must be left alone
	srv := httptest.NewServer(shop.handler())
	defer srv.Close()
	var buf bytes.Buffer
	k := newKitchen(srv, &buf)
	done := make(chan error, 1)
	go func() { done <- k.Run(context.Background()) }()
	waitFor(t, 2*time.Second, func() bool { return shop.totalPosts() >= 2 })
	time.Sleep(100 * time.Millisecond) // many more polls: nothing may be marked twice
	k.Stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	shop.mu.Lock()
	if shop.posts[1] != 1 || shop.posts[2] != 1 || shop.posts[3] != 0 {
		t.Fatalf("posts = %v, want 1,1 and none for other run's order 3", shop.posts)
	}
	shop.mu.Unlock()
	recs := readRecords(t, &buf)
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	for _, r := range recs {
		if r.Kind != record.KindReady || r.Status == nil || *r.Status != 200 || !r.FullResponse ||
			r.ReadySeq == nil || r.OrderID == nil || r.PodUID != "pod-k" || r.Try != 1 {
			t.Errorf("bad ready record %+v", r)
		}
	}
	if got := k.Attempts(); len(got) != 2 || got[0] != recs[0].Attempt && got[0] != recs[1].Attempt {
		t.Errorf("Attempts() = %v, records %v/%v", got, recs[0].Attempt, recs[1].Attempt)
	}
}

func TestKitchenRetriesFailedReadyOnce(t *testing.T) {
	shop := newShop()
	shop.add(1, "runK-c1")
	shop.add(2, "runK-c2")
	shop.failFor[1] = 1  // fails once, retry succeeds
	shop.failFor[2] = 99 // always fails: exactly one retry, then give up
	srv := httptest.NewServer(shop.handler())
	defer srv.Close()
	var buf bytes.Buffer
	k := newKitchen(srv, &buf)
	done := make(chan error, 1)
	go func() { done <- k.Run(context.Background()) }()
	waitFor(t, 2*time.Second, func() bool { return shop.totalPosts() >= 4 })
	time.Sleep(100 * time.Millisecond)
	k.Stop()
	<-done
	shop.mu.Lock()
	if shop.posts[1] != 2 || shop.posts[2] != 2 {
		t.Fatalf("posts = %v, want 2 each", shop.posts)
	}
	shop.mu.Unlock()
	seen := map[string]bool{}
	for _, r := range readRecords(t, &buf) {
		if seen[r.Attempt] {
			t.Errorf("attempt id reused: %s", r.Attempt)
		}
		seen[r.Attempt] = true
		if *r.Status == 503 && r.ReadySeq != nil {
			t.Errorf("503 record has ready_seq: %+v", r)
		}
	}
	if len(seen) != 4 || len(k.Attempts()) != 4 {
		t.Errorf("attempts %d / Attempts() %d, want 4", len(seen), len(k.Attempts()))
	}
}

func TestKitchenDrainStopsAfterMax(t *testing.T) {
	// Every poll shows one more pending order of this run, so there is
	// always work left: Drain must give up after maxWait and stop issuing.
	shop := newShop()
	var id int64
	inner := shop.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			id++
			shop.add(id, fmt.Sprintf("runK-c%d", id))
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	var buf bytes.Buffer
	k := newKitchen(srv, &buf)
	start := time.Now()
	if err := k.Drain(context.Background(), 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 300*time.Millisecond || el > 1500*time.Millisecond {
		t.Fatalf("Drain took %v, want about 300ms", el)
	}
	n := shop.totalPosts()
	if n == 0 {
		t.Fatal("Drain marked nothing")
	}
	time.Sleep(150 * time.Millisecond)
	if got := shop.totalPosts(); got != n {
		t.Fatalf("ready requests kept being issued after Drain: %d -> %d", n, got)
	}
}

func TestKitchenDrainReturnsEarlyWhenNothingLeft(t *testing.T) {
	shop := newShop()
	shop.add(1, "runK-c1")
	srv := httptest.NewServer(shop.handler())
	defer srv.Close()
	var buf bytes.Buffer
	k := newKitchen(srv, &buf)
	start := time.Now()
	if err := k.Drain(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second || shop.totalPosts() != 1 {
		t.Fatalf("took %v, posts %d", time.Since(start), shop.totalPosts())
	}
}

func TestKitchenPropagatesWriterError(t *testing.T) {
	shop := newShop()
	shop.add(1, "runK-c1")
	srv := httptest.NewServer(shop.handler())
	defer srv.Close()
	k := newKitchen(srv, &bytes.Buffer{})
	k.W = record.NewWriter(failWriter{})
	if err := k.Drain(context.Background(), 2*time.Second); err == nil {
		t.Fatal("expected record write error from Drain")
	}
}

func TestKitchenAsksServerForThisRunOnly(t *testing.T) {
	shop := newShop()
	shop.add(1, "runK-c1")
	srv := httptest.NewServer(shop.handler())
	defer srv.Close()
	var buf bytes.Buffer
	k := newKitchen(srv, &buf)
	if err := k.Drain(context.Background(), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	shop.mu.Lock()
	defer shop.mu.Unlock()
	if len(shop.queries) == 0 || shop.queries[0] != "status=pending&run=runK" {
		t.Fatalf("queries = %v", shop.queries)
	}
}

func TestKitchenDrainRetriesFailedPoll(t *testing.T) {
	shop := newShop()
	shop.add(1, "runK-c1")
	shop.getFail = 3 // an error is not an empty list
	srv := httptest.NewServer(shop.handler())
	defer srv.Close()
	var buf bytes.Buffer
	k := newKitchen(srv, &buf)
	if err := k.Drain(context.Background(), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if shop.totalPosts() != 1 {
		t.Fatalf("posts = %d, want 1 (drain gave up on a failed poll)", shop.totalPosts())
	}
}

func TestKitchenDrainReportsPollFailureAtDeadline(t *testing.T) {
	shop := newShop()
	shop.getFail = 1 << 30
	srv := httptest.NewServer(shop.handler())
	defer srv.Close()
	k := newKitchen(srv, &bytes.Buffer{})
	start := time.Now()
	err := k.Drain(context.Background(), 200*time.Millisecond)
	if err == nil || time.Since(start) < 200*time.Millisecond {
		t.Fatalf("err %v after %v, want an error at the deadline", err, time.Since(start))
	}
}

func TestKitchenCancelThenDrainMarksSeenOrders(t *testing.T) {
	shop := newShop()
	shop.add(1, "runK-c1")
	shop.add(2, "runK-c2")
	srv := httptest.NewServer(shop.handler())
	defer srv.Close()
	var buf bytes.Buffer
	k := newKitchen(srv, &buf)
	k.Delay = func() time.Duration { return 400 * time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- k.Run(ctx) }()
	waitFor(t, time.Second, func() bool {
		shop.mu.Lock()
		defer shop.mu.Unlock()
		return shop.gets >= 2 // both orders seen and waiting out their delay
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if shop.totalPosts() != 0 {
		t.Fatal("marked before the delay elapsed")
	}
	k.Delay = func() time.Duration { return time.Millisecond }
	if err := k.Drain(context.Background(), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	shop.mu.Lock()
	defer shop.mu.Unlock()
	if shop.posts[1] != 1 || shop.posts[2] != 1 {
		t.Fatalf("posts = %v, want each order marked once by Drain", shop.posts)
	}
}

func TestKitchenNoRequestStartsAfterStop(t *testing.T) {
	for round := 0; round < 20; round++ {
		shop := newShop()
		for i := int64(1); i <= 40; i++ {
			shop.add(i, fmt.Sprintf("runK-c%d", i))
		}
		srv := httptest.NewServer(shop.handler())
		var buf bytes.Buffer
		k := newKitchen(srv, &buf)
		k.Delay = func() time.Duration { return 0 } // timers fire right as Stop lands
		done := make(chan error, 1)
		go func() { done <- k.Run(context.Background()) }()
		waitFor(t, 2*time.Second, func() bool { return shop.totalPosts() >= 1 })
		k.Stop()
		atStop := len(k.Attempts())
		<-done
		if after := len(k.Attempts()); after != atStop {
			t.Fatalf("round %d: %d attempts issued after Stop returned", round, after-atStop)
		}
		if shop.totalPosts() != atStop {
			t.Fatalf("round %d: %d requests, %d attempts registered at Stop", round, shop.totalPosts(), atStop)
		}
		srv.Close()
	}
}

func TestKitchenDrainTreatsNullPollAsInvalid(t *testing.T) {
	shop := newShop()
	shop.add(1, "runK-c1")
	shop.getNull = 3
	srv := httptest.NewServer(shop.handler())
	defer srv.Close()
	k := newKitchen(srv, &bytes.Buffer{})
	if err := k.Drain(context.Background(), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if shop.totalPosts() != 1 {
		t.Fatalf("posts = %d, want 1 (null body was taken as an empty list)", shop.totalPosts())
	}
}

// slowTransport delays every request before it reaches the server, widening
// the gap between "attempt registered" and "server sees it".
type slowTransport struct{ d time.Duration }

func (s slowTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost {
		time.Sleep(s.d)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestKitchenStopReturnsAfterRegisteredAttemptsFinish(t *testing.T) {
	shop := newShop()
	for i := int64(1); i <= 10; i++ {
		shop.add(i, fmt.Sprintf("runK-c%d", i))
	}
	srv := httptest.NewServer(shop.handler())
	defer srv.Close()
	var buf bytes.Buffer
	k := newKitchen(srv, &buf)
	k.Client = &http.Client{Transport: slowTransport{80 * time.Millisecond}}
	k.Delay = func() time.Duration { return 0 }
	done := make(chan error, 1)
	go func() { done <- k.Run(context.Background()) }()
	waitFor(t, 2*time.Second, func() bool { return len(k.Attempts()) >= 1 })
	k.Stop()
	atStop := shop.totalPosts()
	if got := len(k.Attempts()); got != atStop {
		t.Fatalf("at Stop return: %d attempts registered, server saw %d", got, atStop)
	}
	<-done
	time.Sleep(150 * time.Millisecond)
	if after := shop.totalPosts(); after != atStop {
		t.Fatalf("server-observed requests grew after Stop returned: %d -> %d", atStop, after)
	}
}
