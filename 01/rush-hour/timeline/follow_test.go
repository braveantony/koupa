package timeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRefillVerifiesOverlap(t *testing.T) {
	have := []string{
		"2026-11-09T06:50:00.100Z a",
		"2026-11-09T06:50:01.200Z b",
		"2026-11-09T06:50:01.300Z c",
	}
	// --since-time is truncated to the second: the refill repeats b and c.
	refill := []string{
		"2026-11-09T06:50:01.200Z b",
		"2026-11-09T06:50:01.300Z c",
		"2026-11-09T06:50:02.000Z d",
	}
	newLines, ok := MergeRefill(have, refill)
	if !ok || len(newLines) != 1 || newLines[0] != refill[2] {
		t.Errorf("good refill: new=%v ok=%v", newLines, ok)
	}
	// A line we never saw inside the overlap: the gap is not proven filled.
	bad := []string{"2026-11-09T06:50:01.200Z b", "2026-11-09T06:50:01.250Z lost", "2026-11-09T06:50:01.300Z c", "2026-11-09T06:50:02.000Z d"}
	if _, ok := MergeRefill(have, bad); ok {
		t.Error("refill with an extra line verified")
	}
	// The refill does not reach back to our last line.
	late := []string{"2026-11-09T06:50:02.000Z d"}
	if _, ok := MergeRefill(have, late); ok {
		t.Error("refill without overlap verified")
	}
	// Nothing new, but the overlap matches.
	if n, ok := MergeRefill(have, have[1:]); !ok || len(n) != 0 {
		t.Errorf("empty refill: %v %v", n, ok)
	}
}

func TestFinalizeWritesMeta(t *testing.T) {
	dir := t.TempDir()
	g30 := `,"deletionGracePeriodSeconds":30`
	write(t, filepath.Join(dir, "pods.jsonl"),
		podLine(at(1), "ADDED", "u1", "koupa-a", "")+
			podLine(at(2.3), "MODIFIED", "u1", "koupa-a", fmt.Sprintf(`,"deletionTimestamp":%q%s`, rfc(at(32)), g30))+
			fmt.Sprintf(`%s {"type":"MODIFIED","object":{"kind":"Pod","metadata":{"uid":"u1","name":"koupa-a","deletionTimestamp":%q,"deletionGracePeriodSeconds":0},"status":{"podIP":"10.0.0.7","containerStatuses":[{"name":"koupa","state":{"terminated":{"exitCode":0,"finishedAt":%q}}}]}}}`+"\n",
				stamp(at(4)), rfc(at(4)), "2026-11-09T06:50:03Z"))
	write(t, filepath.Join(dir, "app", "u1.log"),
		fmt.Sprintf("%s %s {\"ts\":%q,\"event\":\"startup\",\"pod_uid\":\"u1\",\"graceful_shutdown\":false}\n", stamp(at(0.5)), rfc(at(-60)), rfc(at(-60)))+
			fmt.Sprintf("%s %s {\"ts\":%q,\"event\":\"sigterm\",\"pod_uid\":\"u1\"}\n", stamp(at(2.5)), rfc(at(2.4)), rfc(at(2.4))))
	st := FollowState{PodUID: "u1", PodName: "koupa-a", CollectStart: *at(0), StartupFromBeginning: true, Ended: "container_exited"}
	b, _ := json.Marshal(st)
	write(t, filepath.Join(dir, "app", "u1.collect.json"), string(b))
	write(t, filepath.Join(dir, "pods-at-stop.json"), `{"items":[]}`)

	if err := Finalize(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "app", "u1.meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		PodUID    string  `json:"pod_uid"`
		PodName   string  `json:"pod_name"`
		Complete  bool    `json:"complete"`
		ExitedAt  *string `json:"exited_at"`
		TermStart *string `json:"term_start"`
		PodIP     string  `json:"pod_ip"`
		Prec      *int64  `json:"term_start_precision_ms"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.PodUID != "u1" || m.PodName != "koupa-a" || !m.Complete || m.PodIP != "10.0.0.7" {
		t.Errorf("meta: %s", raw)
	}
	if m.TermStart == nil || *m.TermStart != rfc(at(2)) {
		t.Errorf("term_start: %s", raw)
	}
	if m.ExitedAt == nil || *m.ExitedAt != "2026-11-09T06:50:03Z" {
		t.Errorf("exited_at: %s", raw)
	}
	if m.Prec == nil || *m.Prec < 1000 {
		t.Errorf("precision: %s", raw)
	}
	// Idempotent: a second finalize gives the same file.
	if err := Finalize(dir); err != nil {
		t.Fatal(err)
	}
	raw2, _ := os.ReadFile(filepath.Join(dir, "app", "u1.meta.json"))
	if string(raw2) != string(raw) {
		t.Errorf("second finalize changed meta:\n%s\n%s", raw, raw2)
	}
}

// fakeKubectl: the first follow read breaks (rc 1) after two lines; the
// refill repeats the overlap and adds one line, then the container exits;
// the drain read gets the kubelet's "unable to retrieve" answer.
const fakeKubectl = `#!/bin/sh
d=$FAKE_DIR
case "$*" in
*"get pod"*)
	if [ -e "$d/exited" ]; then s='"terminated":{"exitCode":0,"finishedAt":"2026-11-09T06:50:03Z"}'; else s='"running":{}'; fi
	echo '{"metadata":{"uid":"u1","name":"koupa-a"},"status":{"containerStatuses":[{"name":"koupa","state":{'"$s"'}}]}}'
	;;
*"logs"*"-f"*"--since-time=2026-11-09T06:50:01Z"*)
	echo '2026-11-09T06:50:01.200Z {"event":"request_end","pod_uid":"u1"}'
	echo '2026-11-09T06:50:02.000Z {"ts":"2026-11-09T06:50:02Z","event":"sigterm","pod_uid":"u1"}'
	touch "$d/exited"
	;;
*"logs"*"-f"*)
	echo '2026-11-09T06:50:00.100Z {"ts":"2026-11-09T06:50:00.1Z","event":"startup","pod_uid":"u1","graceful_shutdown":false}'
	echo '2026-11-09T06:50:01.200Z {"event":"request_end","pod_uid":"u1"}'
	exit 1
	;;
*"logs"*)
	echo 'unable to retrieve container logs for cri-o://abc'
	;;
esac
`

func TestFollowerRefillsBreakAndDrains(t *testing.T) {
	dir := t.TempDir()
	kc := filepath.Join(dir, "kubectl")
	write(t, kc, fakeKubectl)
	if err := os.Chmod(kc, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_DIR", dir)
	app := filepath.Join(dir, "app")
	f := Follower{Kubectl: kc, Namespace: "koupa", Pod: "koupa-a", UID: "u1", Dir: app, Poll: time.Millisecond}
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.Run(ctx); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(app, "u1.collect.json"))
	var st FollowState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Ended != "container_exited" || !st.StartupFromBeginning || st.CollectEnd == nil {
		t.Errorf("state: %s", raw)
	}
	if len(st.Gaps) != 1 || !st.Gaps[0].Refilled || !st.Gaps[0].Verified {
		t.Errorf("want one verified gap: %s", raw)
	}
	if !strings.Contains(st.DrainCheck, "沒有回傳") {
		t.Errorf("drain check: %q", st.DrainCheck)
	}
	logb, _ := os.ReadFile(filepath.Join(app, "u1.log"))
	lines := strings.Split(strings.TrimSpace(string(logb)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], "sigterm") || strings.Contains(string(logb), "unable") {
		t.Errorf("log:\n%s", logb)
	}
	for _, l := range lines {
		if _, _, ok := splitStamp([]byte(l)); !ok {
			t.Errorf("line without collector stamp: %s", l)
		}
	}
	m, notes := LoadPodLogMeta(dir, "u1", nil, map[string]*podObj{}, true)
	if r := Coverage(m); !r.Complete || len(notes) > 0 {
		t.Errorf("coverage after a verified refill: %+v %v", r, notes)
	}
}
