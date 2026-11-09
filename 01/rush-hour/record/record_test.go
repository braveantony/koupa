package record

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAttemptJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	in := Attempt{
		Run: "r1", Kind: KindOrder, ClientRef: "r1-c1", OrderID: i64(7), Attempt: "r1-a1-t1", Try: 1,
		ScheduledAt: now, SentAt: now.Add(3 * time.Millisecond), RespondedAt: now.Add(10 * time.Millisecond),
		Status: ip(201), ErrKind: "", PodUID: "pod-x", ReadySeq: i64(4), SchedLagMs: 3, FullResponse: true,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"run", "kind", "client_ref", "order_id", "attempt", "try", "scheduled_at", "sent_at",
		"responded_at", "status", "err_kind", "pod_uid", "ready_seq", "sched_lag_ms", "full_response"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing field %q in %s", k, b)
		}
	}
	var out Attempt
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !out.SentAt.Equal(in.SentAt) || out.ClientRef != in.ClientRef || out.Try != 1 || !out.FullResponse || out.SchedLagMs != 3 {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestWriterConcurrentOneLinePerRecord(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := w.Write(Attempt{Run: "r", Kind: KindOrder, Try: i}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 50 {
		t.Fatalf("want 50 lines, got %d", len(lines))
	}
	for _, l := range lines {
		var a Attempt
		if err := json.Unmarshal([]byte(l), &a); err != nil {
			t.Fatalf("bad line %q: %v", l, err)
		}
	}
}

func i64(v int64) *int64 { return &v }
func ip(v int) *int      { return &v }

func TestAbsentDiffersFromZero(t *testing.T) {
	b, _ := json.Marshal(Attempt{Run: "r"})
	for _, k := range []string{"order_id", "status", "ready_seq"} {
		if strings.Contains(string(b), `"`+k+`"`) {
			t.Errorf("absent %s serialized: %s", k, b)
		}
	}
	b, _ = json.Marshal(Attempt{Run: "r", OrderID: i64(0), Status: ip(0), ReadySeq: i64(0)})
	var out Attempt
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.OrderID == nil || out.Status == nil || out.ReadySeq == nil {
		t.Fatalf("zero lost: %s", b)
	}
}

type failW struct{}

func (failW) Write([]byte) (int, error) { return 0, errors.New("disk full") }

type shortW struct{}

func (shortW) Write(b []byte) (int, error) { return len(b) - 1, nil }

type flushW struct {
	bytes.Buffer
	flushed int
}

func (f *flushW) Flush() error { f.flushed++; return nil }

func TestWriterStickyError(t *testing.T) {
	w := NewWriter(failW{})
	if err := w.Write(Attempt{}); err == nil {
		t.Fatal("want error")
	}
	if w.Err() == nil {
		t.Fatal("Err not sticky")
	}
}

func TestWriterShortWriteIsError(t *testing.T) {
	w := NewWriter(shortW{})
	if err := w.Write(Attempt{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("got %v", err)
	}
	if w.Err() == nil {
		t.Fatal("Err not set")
	}
}

func TestWriterFlushesEachRecord(t *testing.T) {
	f := &flushW{}
	w := NewWriter(f)
	w.Write(Attempt{})
	w.Write(Attempt{})
	if f.flushed != 2 || w.Err() != nil {
		t.Fatalf("flushed %d err %v", f.flushed, w.Err())
	}
}
