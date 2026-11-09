package record

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAttemptJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	in := Attempt{
		Run: "r1", Kind: KindOrder, ClientRef: "r1-c1", OrderID: 7, Attempt: "r1-a1-t1", Try: 1,
		ScheduledAt: now, SentAt: now.Add(3 * time.Millisecond), RespondedAt: now.Add(10 * time.Millisecond),
		Status: 201, ErrKind: "", PodUID: "pod-x", ReadySeq: 4, SchedLagMs: 3, FullResponse: true,
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
