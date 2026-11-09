package timeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stamp(t *time.Time) string { return fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond()) }

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rfc(t *time.Time) string { return t.Format(time.RFC3339Nano) }

// podLine is one stamped `kubectl get pods -w -o json --output-watch-events` line.
func podLine(collected *time.Time, typ, uid, name, extra string) string {
	return fmt.Sprintf(`%s {"type":%q,"object":{"kind":"Pod","metadata":{"uid":%q,"name":%q,"labels":{"app.kubernetes.io/name":"koupa"}%s},"status":{}}}`+"\n",
		stamp(collected), typ, uid, name, extra)
}

func TestMissingSourceMarked(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "pods.jsonl"),
		podLine(at(1), "ADDED", "u1", "koupa-a", "")+
			podLine(at(2.3), "MODIFIED", "u1", "koupa-a", fmt.Sprintf(`,"deletionTimestamp":%q,"deletionGracePeriodSeconds":30`, rfc(at(32)))))
	write(t, filepath.Join(dir, "missing.jsonl"), `{"source":"hubble","reason":"exec failed"}`+"\n")
	evs, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	var missing = map[string]string{}
	var del bool
	for _, e := range evs {
		if e.Kind == KindMissing {
			missing[e.Source] = e.Detail
		}
		if e.Kind == KindDeletionStart && e.PodUID == "u1" {
			del = true
			if e.EventTime != nil || e.IntervalStart == nil || !e.IntervalStart.Equal(*at(2)) || !e.IntervalEnd.Equal(*at(2.3)) {
				t.Errorf("deletion start should be the interval [2s, 2.3s]: %+v", e)
			}
		}
	}
	if !del {
		t.Error("the other sources were dropped: no deletion start")
	}
	if !strings.Contains(missing["hubble"], "exec failed") {
		t.Errorf("hubble not marked with its reason: %v", missing)
	}
	for _, src := range []string{"endpointslices", "events", "ingress", "app"} {
		if _, ok := missing[src]; !ok {
			t.Errorf("absent source %s not marked missing: %v", src, missing)
		}
	}
	// The old Pod has no SIGTERM or exit evidence: the text says so.
	if err := Write(dir, evs); err != nil {
		t.Fatal(err)
	}
	txt, _ := os.ReadFile(filepath.Join(dir, "timeline.txt"))
	if !strings.Contains(string(txt), "缺少") || !strings.Contains(string(txt), "hubble") {
		t.Errorf("timeline.txt does not mark missing sources:\n%s", txt)
	}
}

func TestTimelineKeepsBothTimes(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "app", "u1.log"),
		fmt.Sprintf("%s %s {\"ts\":%q,\"event\":\"sigterm\",\"pod_uid\":\"u1\"}\n", stamp(at(0.5)), rfc(at(0.1)), rfc(at(0))))
	evs, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got *Event
	for i := range evs {
		if evs[i].Kind == KindSigterm {
			got = &evs[i]
		}
	}
	if got == nil {
		t.Fatalf("no sigterm event: %+v", evs)
	}
	if got.EventTime == nil || !got.EventTime.Equal(*at(0)) {
		t.Errorf("event_time = %v, want the App's ts", got.EventTime)
	}
	if got.CollectedAt == nil || !got.CollectedAt.Equal(*at(0.5)) {
		t.Errorf("collected_at = %v, want the collector stamp", got.CollectedAt)
	}
	if got.Source != "app" || got.PodUID != "u1" {
		t.Errorf("source/pod: %+v", got)
	}
	if err := Write(dir, evs); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "timeline.jsonl"))
	for _, l := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatal(err)
		}
		if m["kind"] == KindSigterm {
			if m["event_time"] == nil || m["collected_at"] == nil || m["source"] != "app" {
				t.Errorf("jsonl lost a time: %s", l)
			}
			return
		}
	}
	t.Error("sigterm not in timeline.jsonl")
}

func TestDeletionWithoutGraceIsMarkedUnknown(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "pods.jsonl"),
		podLine(at(1), "ADDED", "u1", "koupa-a", "")+
			podLine(at(2.3), "MODIFIED", "u1", "koupa-a", fmt.Sprintf(`,"deletionTimestamp":%q`, rfc(at(32)))))
	evs, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind == KindDeletionStart {
			if e.IntervalStart != nil || e.EventTime != nil || !strings.Contains(e.Detail, "未知") {
				t.Errorf("deletion without grace: %+v", e)
			}
			return
		}
	}
	t.Error("observed deletion dropped from the timeline")
}
