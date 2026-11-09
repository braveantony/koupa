package load

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"koupa/internal/wire"
	"koupa/rush-hour/record"
)

func readRecords(t *testing.T, buf *bytes.Buffer) []record.Attempt {
	t.Helper()
	var out []record.Attempt
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var a record.Attempt
		if err := json.Unmarshal([]byte(l), &a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func TestCustomerRetriesSameClientRef(t *testing.T) {
	var mu sync.Mutex
	var bodies, attempts []string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		n++
		cur := n
		bodies = append(bodies, string(b))
		attempts = append(attempts, r.Header.Get(wire.HeaderAttempt))
		mu.Unlock()
		w.Header().Set(wire.HeaderPodUID, "pod-1")
		if cur == 1 {
			http.Error(w, "boom", 503)
			return
		}
		w.WriteHeader(201)
		io.WriteString(w, `{"order_id":42,"number":3,"total":900}`)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	c := &Customer{Client: srv.Client(), Base: srv.URL, Run: "run1", Timeout: 2 * time.Second, Retries: 1,
		W: record.NewWriter(&buf), MenuIDs: []int{1, 2, 3, 4}, Seed: 1}
	c.Place(context.Background(), 7, time.Now())

	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("bodies: %v", bodies)
	}
	if attempts[0] != "run1-a7-t1" || attempts[1] != "run1-a7-t2" {
		t.Fatalf("attempt ids: %v", attempts)
	}
	var req struct {
		ClientRef string `json:"client_ref"`
		Items     []struct {
			MenuItemID int `json:"menu_item_id"`
			Quantity   int `json:"quantity"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &req); err != nil {
		t.Fatal(err)
	}
	if req.ClientRef != "run1-c7" {
		t.Fatalf("client_ref %q", req.ClientRef)
	}
	q := 0
	for _, it := range req.Items {
		q += it.Quantity
	}
	if q < 1 || q > 4 {
		t.Fatalf("total quantity %d", q)
	}
	recs := readRecords(t, &buf)
	if len(recs) != 2 {
		t.Fatalf("records %d", len(recs))
	}
	if *recs[0].Status != 503 || !recs[0].FullResponse || recs[0].Try != 1 || recs[0].PodUID != "pod-1" {
		t.Fatalf("rec0 %+v", recs[0])
	}
	if *recs[1].Status != 201 || recs[1].Try != 2 || recs[1].OrderID == nil || *recs[1].OrderID != 42 || recs[1].ClientRef != "run1-c7" || recs[1].Attempt != "run1-a7-t2" {
		t.Fatalf("rec1 %+v", recs[1])
	}
	if recs[0].Kind != record.KindOrder || recs[0].Run != "run1" {
		t.Fatalf("rec0 %+v", recs[0])
	}
}

func TestCustomerRetriesOnClientErrorToo(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		http.Error(w, "conflict", 409)
	}))
	defer srv.Close()
	var buf bytes.Buffer
	c := &Customer{Client: srv.Client(), Base: srv.URL, Run: "r", Timeout: time.Second, Retries: 1,
		W: record.NewWriter(&buf), MenuIDs: []int{1}, Seed: 1}
	c.Place(context.Background(), 1, time.Now())
	if n != 2 {
		t.Fatalf("requests %d, want 2 (retry once on any non-2xx)", n)
	}
}

func TestCustomerRecordsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	var buf bytes.Buffer
	c := &Customer{Client: srv.Client(), Base: srv.URL, Run: "r", Timeout: 100 * time.Millisecond, Retries: 1,
		W: record.NewWriter(&buf), MenuIDs: []int{1}, Seed: 1}
	c.Place(context.Background(), 1, time.Now())
	recs := readRecords(t, &buf)
	if len(recs) != 2 {
		t.Fatalf("records %d", len(recs))
	}
	for _, r := range recs {
		if r.ErrKind != "timeout" || r.FullResponse || r.Status != nil || r.OrderID != nil {
			t.Fatalf("%+v", r)
		}
	}
}

func TestCustomerRecordsRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	var buf bytes.Buffer
	c := &Customer{Client: &http.Client{}, Base: base, Run: "r", Timeout: time.Second, Retries: 1,
		W: record.NewWriter(&buf), MenuIDs: []int{1}, Seed: 1}
	c.Place(context.Background(), 1, time.Now())
	recs := readRecords(t, &buf)
	if len(recs) != 2 || recs[0].ErrKind != "refused" {
		t.Fatalf("%+v", recs)
	}
}

func TestCustomerRecordsReset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()
	var buf bytes.Buffer
	c := &Customer{Client: &http.Client{}, Base: srv.URL, Run: "r", Timeout: time.Second, Retries: 0,
		W: record.NewWriter(&buf), MenuIDs: []int{1}, Seed: 1}
	c.Place(context.Background(), 1, time.Now())
	recs := readRecords(t, &buf)
	if len(recs) != 1 || recs[0].ErrKind != "reset" || recs[0].FullResponse {
		t.Fatalf("%+v", recs)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestCustomerPropagatesWriteError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) }))
	defer srv.Close()
	w := record.NewWriter(failWriter{})
	c := &Customer{Client: srv.Client(), Base: srv.URL, Run: "r", Timeout: time.Second, Retries: 1,
		W: w, MenuIDs: []int{1}, Seed: 1}
	if err := c.Place(context.Background(), 1, time.Now()); err == nil {
		t.Fatal("Place swallowed write error")
	}
	if w.Err() == nil {
		t.Fatal("writer Err not set")
	}
}
