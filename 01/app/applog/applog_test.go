package applog

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLogLineShape(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "uid-1", "pod-1", "v1")
	log.Info(EvStartup, "extra", 7)

	out := buf.String()
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("want exactly one line, got %q", out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(err)
	}
	if m["event"] != EvStartup || m["pod_uid"] != "uid-1" || m["pod_name"] != "pod-1" || m["version"] != "v1" {
		t.Errorf("fields wrong: %v", m)
	}
	ts, ok := m["ts"].(string)
	if !ok {
		t.Fatalf("ts missing: %v", m)
	}
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Errorf("ts not RFC3339Nano: %v", err)
	}
	if _, has := m["time"]; has {
		t.Error("default time key must be replaced")
	}
	if _, has := m["msg"]; has {
		t.Error("default msg key must be replaced")
	}
	if m["extra"] != float64(7) {
		t.Errorf("extra attr lost: %v", m)
	}
}

func TestEveryEventCarriesPodUID(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "uid-9", "p", "v2")
	for _, ev := range []string{EvStartup, EvTxTimeoutCheck, EvRequestReceived, EvRequestEnd, EvSigterm,
		EvExit, EvCommitUnknown, EvWriteStart, EvWriteEnd, EvWriteBlocked, EvMutationDropReady,
		EvMutationDuplicateOrder, EvHookPaused, EvShutdownTimeout, EvProgressAheadOfH} {
		buf.Reset()
		log.Info(ev)
		var m map[string]any
		if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		if m["event"] != ev || m["pod_uid"] != "uid-9" {
			t.Errorf("%s: %v", ev, m)
		}
	}
}
