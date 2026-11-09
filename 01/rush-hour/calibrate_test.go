package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"koupa/rush-hour/record"
)

// calibPod is one Pod of a synthetic calibrate run: the gateway may pick it
// from From until Gone; Del is its estimated termination start (nil: not
// terminated during the run).
type calibPod struct {
	UID, Name      string
	From, Gone     time.Duration
	Del            *time.Duration
	NeverAfterDel  bool // the gateway stops picking it at Del exactly
	EmptyUIDOnHits bool // its 200 responses carry no Pod UID
}

func dur(d time.Duration) *time.Duration { return &d }

var calibT0 = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

// writeCalibRun writes DIR/probes.jsonl (50/s for 60 s, each probe picks a
// selectable Pod uniformly at random) and DIR/app/<uid>.meta.json.
func writeCalibRun(t *testing.T, dir string, seed int64, pods []calibPod) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "probes.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	w := record.NewWriter(f)
	rng := rand.New(rand.NewSource(seed))
	ok := 200
	for i := 0; i < 50*60; i++ {
		at := time.Duration(i) * 20 * time.Millisecond
		var sel []calibPod
		for _, p := range pods {
			gone := p.Gone
			if p.NeverAfterDel && p.Del != nil {
				gone = *p.Del
			}
			if at >= p.From && at < gone {
				sel = append(sel, p)
			}
		}
		// SentAt 0.7 ms after the tick, so a millisecond-truncated ingress
		// start_time can fall before it (writeIngressLog).
		rec := record.Attempt{Run: filepath.Base(dir), Kind: record.KindProbe, Try: 1,
			Attempt:     fmt.Sprintf("%s-p-a%d-t1", filepath.Base(dir), i+1),
			ScheduledAt: calibT0.Add(at), SentAt: calibT0.Add(at + 700*time.Microsecond)}
		if i%97 == 5 || len(sel) == 0 { // an occasional probe without response
			rec.ErrKind = record.ErrTimeout
		} else {
			p := sel[rng.Intn(len(sel))]
			rec.RespondedAt = calibT0.Add(at + 4*time.Millisecond)
			rec.Status = &ok
			if !p.EmptyUIDOnHits {
				rec.PodUID = p.UID
			}
		}
		if err := w.Write(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range pods {
		m := map[string]any{"pod_uid": p.UID, "pod_name": p.Name, "term_start": nil}
		if p.Del != nil {
			m["term_start"] = calibT0.Add(*p.Del).Format(time.RFC3339Nano)
		}
		b, _ := json.Marshal(m)
		if err := os.WriteFile(filepath.Join(dir, "app", p.UID+".meta.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

type calibOut struct {
	PreStopS    float64  `json:"prestop_s"`
	Upper       string   `json:"upper_bound_method"`
	Valid       int      `json:"valid_samples"`
	CandidateS  *float64 `json:"candidate_s"`
	ProbesNoUID int      `json:"probes_without_pod_uid"`
	Samples     []struct {
		Run       string   `json:"run"`
		PodUID    string   `json:"pod_uid"`
		PodName   string   `json:"pod_name"`
		Lower     *string  `json:"lower"`
		Upper     *string  `json:"upper"`
		UpperKind string   `json:"upper_kind"`
		P         float64  `json:"p"`
		K         int      `json:"k"`
		N         int      `json:"n"`
		UMinusDel *float64 `json:"u_minus_deletion_s"`
		Valid     bool     `json:"valid"`
		Reason    string   `json:"invalid_reason"`
	} `json:"samples"`
}

func runCalibrate(t *testing.T, args ...string) (calibOut, string) {
	t.Helper()
	var out, errb bytes.Buffer
	if code := run(append([]string{"calibrate"}, args...), &out, &errb); code != 0 {
		t.Fatalf("calibrate exit %d: %s", code, errb.String())
	}
	var c calibOut
	jsonPath := ""
	for i, a := range args {
		if a == "--json" {
			jsonPath = args[i+1]
		}
	}
	b, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c, out.String()
}

// Three runs, two terminated Pods each that the gateway keeps picking for
// 2 s after the delete request: 6 valid model samples, candidate ~2 s.
func TestCalibrateCandidateFromRuns(t *testing.T) {
	root := t.TempDir()
	var dirs []string
	for r := 0; r < 3; r++ {
		dir := filepath.Join(root, "calibrate-r"+string(rune('1'+r)))
		dirs = append(dirs, dir)
		writeCalibRun(t, dir, int64(r+1), []calibPod{
			{UID: "a" + dir[len(dir)-1:], Name: "koupa-a", From: 0, Gone: 22 * time.Second, Del: dur(20 * time.Second)},
			{UID: "b" + dir[len(dir)-1:], Name: "koupa-b", From: 0, Gone: 36 * time.Second, Del: dur(34 * time.Second)},
			{UID: "c" + dir[len(dir)-1:], Name: "koupa-c", From: 0, Gone: time.Hour},
			{UID: "n" + dir[len(dir)-1:], Name: "koupa-n", From: 15 * time.Second, Gone: time.Hour},
		})
	}
	jp := filepath.Join(root, "calibrate.json")
	c, text := runCalibrate(t, append([]string{"--prestop", "20s", "--json", jp}, dirs...)...)
	if len(c.Samples) != 6 {
		t.Fatalf("samples = %d, want 6 (one per terminated Pod)", len(c.Samples))
	}
	if c.Valid != 6 || c.CandidateS == nil {
		t.Fatalf("valid %d, candidate %v; samples %+v", c.Valid, c.CandidateS, c.Samples)
	}
	if *c.CandidateS < 2 || *c.CandidateS > 3.5 {
		t.Errorf("candidate %.3f s, want just above 2 s", *c.CandidateS)
	}
	if c.PreStopS != 20 || c.Upper != "model" {
		t.Errorf("prestop %v, upper method %q", c.PreStopS, c.Upper)
	}
	for _, s := range c.Samples {
		if s.UpperKind != "model" || s.K < 1 || s.N < 100 || s.P <= 0 || s.Lower == nil || s.Upper == nil {
			t.Errorf("sample %+v", s)
		}
	}
	for _, want := range []string{"koupa-a", "model", "valid", "candidate"} {
		if !strings.Contains(text, want) {
			t.Errorf("table lacks %q:\n%s", want, text)
		}
	}
}

// A Pod the gateway dropped at once (no hit after the delete request) is not
// a valid sample; fewer than 5 valid samples give no candidate.
func TestCalibrateNoHitAfterDeletionAndTooFew(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "r1")
	writeCalibRun(t, dir, 7, []calibPod{
		{UID: "a", Name: "koupa-a", Gone: time.Hour, Del: dur(20 * time.Second), NeverAfterDel: true},
		{UID: "b", Name: "koupa-b", Gone: 32 * time.Second, Del: dur(30 * time.Second)},
		{UID: "c", Name: "koupa-c", Gone: time.Hour},
	})
	jp := filepath.Join(root, "out.json")
	c, text := runCalibrate(t, "--json", jp, dir)
	if len(c.Samples) != 2 || c.Valid != 1 || c.CandidateS != nil {
		t.Fatalf("samples %d valid %d candidate %v", len(c.Samples), c.Valid, c.CandidateS)
	}
	byName := map[string]string{}
	for _, s := range c.Samples {
		byName[s.PodName] = s.Reason
	}
	if byName["koupa-a"] != "no_hit_after_deletion" || byName["koupa-b"] != "" {
		t.Errorf("reasons %v", byName)
	}
	if !strings.Contains(text, "no_hit_after_deletion") || !strings.Contains(text, "no candidate") {
		t.Errorf("table:\n%s", text)
	}
}

// 200 responses without a Pod UID are counted (A11: every probe response
// carries the Pod UID); a run directory without probes.jsonl is an error.
func TestCalibrateCountsProbesWithoutUIDAndRejectsMissingProbes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "r1")
	writeCalibRun(t, dir, 3, []calibPod{
		{UID: "a", Name: "koupa-a", Gone: 22 * time.Second, Del: dur(20 * time.Second)},
		{UID: "x", Name: "koupa-x", Gone: time.Hour, EmptyUIDOnHits: true},
	})
	c, _ := runCalibrate(t, "--json", filepath.Join(root, "o.json"), dir)
	if c.ProbesNoUID == 0 {
		t.Errorf("probes_without_pod_uid = 0")
	}
	empty := filepath.Join(root, "empty")
	os.MkdirAll(empty, 0o755)
	var out, errb bytes.Buffer
	if code := run([]string{"calibrate", "--json", filepath.Join(root, "e.json"), empty}, &out, &errb); code != 2 {
		t.Errorf("missing probes.jsonl: exit %d", code)
	}
	if code := run([]string{"calibrate"}, &out, &errb); code != 2 {
		t.Errorf("no dir: exit %d", code)
	}
}

// ingressFixture describes the access log written next to a synthetic run.
type ingressFixture struct {
	IPs map[string]string // pod UID -> Pod IP (also written to the meta files)
	// Extra lines (non-probe traffic): upstream Pod UID and start offset.
	Extra []struct {
		UID string
		At  time.Duration
	}
	// SkewAttempt: this probe's line gets start_time = sent - 5 ms (a real
	// clock disagreement, beyond the 1 ms truncation of start_time).
	SkewAttempt string
	// DropAttempt: no access-log line for this probe.
	DropAttempt string
}

// writeIngressLog reads DIR/probes.jsonl and writes DIR/ingress.log the way
// collect-timeline.sh stores Envoy's access log: "<collected> {json}", with
// start_time truncated to the millisecond. The ingress selects a probe
// 0.2 ms after SentAt for odd sequence numbers (truncation then puts
// start_time before SentAt) and 2 ms after it for even ones.
func writeIngressLog(t *testing.T, dir string, fx ingressFixture) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "probes.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	line := func(attempt, path, uid string, start time.Time) {
		m := map[string]any{"attempt": attempt, "duration_ms": 1, "method": "GET", "path": path,
			"response_code_details": "via_upstream", "response_flags": "-",
			"start_time": start.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z"),
			"status":     200, "upstream_attempts": 1, "upstream_host": fx.IPs[uid] + ":8080"}
		if attempt == "" {
			m["attempt"] = nil
		}
		j, _ := json.Marshal(m)
		sb.WriteString("1794219519.245897834 ")
		sb.Write(j)
		sb.WriteByte('\n')
	}
	for i, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var a record.Attempt
		if err := json.Unmarshal([]byte(l), &a); err != nil {
			t.Fatal(err)
		}
		if a.PodUID == "" || a.Attempt == fx.DropAttempt {
			continue
		}
		sel := a.SentAt.Add(2 * time.Millisecond)
		if (i+1)%2 == 1 {
			sel = a.SentAt.Add(200 * time.Microsecond)
		}
		if a.Attempt == fx.SkewAttempt {
			sel = a.SentAt.Add(-5 * time.Millisecond)
		}
		line(a.Attempt, "/api/whoami", a.PodUID, sel)
	}
	for _, e := range fx.Extra {
		line("", "/api/orders?status=pending", e.UID, calibT0.Add(e.At))
	}
	if err := os.WriteFile(filepath.Join(dir, "ingress.log"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	for uid, ip := range fx.IPs {
		mp := filepath.Join(dir, "app", uid+".meta.json")
		var m map[string]any
		mb, err := os.ReadFile(mp)
		if err != nil {
			t.Fatal(err)
		}
		json.Unmarshal(mb, &m)
		m["pod_ip"] = ip
		mb, _ = json.Marshal(m)
		os.WriteFile(mp, mb, 0o644)
	}
}

type calibIngressOut struct {
	PerAttempt    bool `json:"per_attempt_ingress"`
	NoIngressLine int  `json:"probes_without_ingress_line"`
	IngressRuns   int  `json:"runs_with_ingress_log"`
	Samples       []struct {
		PodName    string   `json:"pod_name"`
		Lower      *string  `json:"lower"`
		LSource    string   `json:"lower_source"`
		LMinusDel  *float64 `json:"l_minus_deletion_s"`
		LastAnyDel *float64 `json:"last_ingress_any_minus_deletion_s"`
		Valid      bool     `json:"valid"`
		Reason     string   `json:"invalid_reason"`
	} `json:"samples"`
}

func readIngressOut(t *testing.T, path string) calibIngressOut {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c calibIngressOut
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// lastHit returns the last probe that the Pod answered.
func lastHit(t *testing.T, dir, uid string) (record.Attempt, int) {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, "probes.jsonl"))
	var last record.Attempt
	seq := 0
	for i, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var a record.Attempt
		json.Unmarshal([]byte(l), &a)
		if a.PodUID == uid {
			last, seq = a, i+1
		}
	}
	return last, seq
}

// With DIR/ingress.log, L is the access-log start_time of the last probe the
// ingress sent to the Pod (A11 step 3, per-attempt ingress record). A
// start_time that is before SentAt by less than its 1 ms truncation is
// clamped to SentAt (both are lower bounds of the selection time); probes
// without a line fall back to SentAt and are counted. The last selection by
// any request (not only probes) is reported as a cross-reference.
func TestCalibrateUsesPerAttemptIngress(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "r1")
	writeCalibRun(t, dir, 11, []calibPod{
		{UID: "a", Name: "koupa-a", Gone: 22 * time.Second, Del: dur(20 * time.Second)},
		{UID: "b", Name: "koupa-b", Gone: 37 * time.Second, Del: dur(35 * time.Second)},
		{UID: "c", Name: "koupa-c", Gone: time.Hour},
	})
	hitA, _ := lastHit(t, dir, "a")
	fx := ingressFixture{IPs: map[string]string{"a": "10.0.0.1", "b": "10.0.0.2", "c": "10.0.0.3"},
		DropAttempt: hitA.Attempt}
	fx.Extra = append(fx.Extra, struct {
		UID string
		At  time.Duration
	}{"a", 23500 * time.Millisecond})
	writeIngressLog(t, dir, fx)
	// a's last probe line was dropped: the hit before it gives L when it has
	// a line, else SentAt; b keeps its last line.
	hitB, seqB := lastHit(t, dir, "b")
	wantB := hitB.SentAt.Add(2 * time.Millisecond).Truncate(time.Millisecond)
	if seqB%2 == 1 {
		wantB = hitB.SentAt // start_time truncated below SentAt -> clamp
	}

	jp := filepath.Join(root, "o.json")
	runCalibrate(t, "--json", jp, dir)
	c := readIngressOut(t, jp)
	if !c.PerAttempt || c.IngressRuns != 1 {
		t.Errorf("per_attempt_ingress %v, runs_with_ingress_log %d", c.PerAttempt, c.IngressRuns)
	}
	if c.NoIngressLine != 1 {
		t.Errorf("probes_without_ingress_line = %d, want 1", c.NoIngressLine)
	}
	got := map[string]int{}
	for i, s := range c.Samples {
		got[s.PodName] = i
	}
	a, b := c.Samples[got["koupa-a"]], c.Samples[got["koupa-b"]]
	if !a.Valid || !b.Valid {
		t.Fatalf("samples not valid (truncation must not count as inconsistent timing): %+v", c.Samples)
	}
	if b.Lower == nil || *b.Lower != wantB.UTC().Format(time.RFC3339Nano) || b.LSource != "ingress" {
		t.Errorf("b: lower %v source %q, want %s ingress", b.Lower, b.LSource, wantB.UTC().Format(time.RFC3339Nano))
	}
	if a.Lower == nil || *a.Lower != hitA.SentAt.UTC().Format(time.RFC3339Nano) || a.LSource != "sent" {
		t.Errorf("a: lower %v source %q, want SentAt of the probe without a line %s", a.Lower, a.LSource,
			hitA.SentAt.UTC().Format(time.RFC3339Nano))
	}
	if b.LMinusDel == nil || abs(*b.LMinusDel-wantB.Sub(calibT0.Add(35*time.Second)).Seconds()) > 1e-6 {
		t.Errorf("b: l_minus_deletion_s %v", b.LMinusDel)
	}
	if a.LastAnyDel == nil || abs(*a.LastAnyDel-3.5) > 1e-6 {
		t.Errorf("a: last_ingress_any_minus_deletion_s %v, want 3.5 (the non-probe line)", a.LastAnyDel)
	}
	if b.LastAnyDel == nil || abs(*b.LastAnyDel-wantB.Sub(calibT0.Add(35*time.Second)).Seconds()) > 0.003 {
		t.Errorf("b: last_ingress_any_minus_deletion_s %v", b.LastAnyDel)
	}
	var out, errb bytes.Buffer
	run([]string{"calibrate", dir}, &out, &errb)
	for _, want := range []string{"L 取法：逐次入口紀錄", "L-del(s)", "last-any(s)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("table lacks %q:\n%s", want, out.String())
		}
	}
}

// start_time earlier than SentAt by more than the truncation is a clock
// disagreement: the run's samples are inconsistent_timing. Without
// ingress.log, L stays the send time and per_attempt_ingress is false.
func TestCalibrateIngressSkewAndNoLog(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "r1")
	pods := []calibPod{
		{UID: "a", Name: "koupa-a", Gone: 22 * time.Second, Del: dur(20 * time.Second)},
		{UID: "c", Name: "koupa-c", Gone: time.Hour},
	}
	writeCalibRun(t, dir, 5, pods)
	hitA, _ := lastHit(t, dir, "a")
	writeIngressLog(t, dir, ingressFixture{IPs: map[string]string{"a": "10.0.0.1", "c": "10.0.0.3"},
		SkewAttempt: hitA.Attempt})
	jp := filepath.Join(root, "o.json")
	runCalibrate(t, "--json", jp, dir)
	c := readIngressOut(t, jp)
	if len(c.Samples) != 1 || c.Samples[0].Reason != "inconsistent_timing" {
		t.Errorf("skewed start_time: %+v", c.Samples)
	}

	plain := filepath.Join(root, "r2")
	writeCalibRun(t, plain, 5, pods)
	jp2 := filepath.Join(root, "o2.json")
	runCalibrate(t, "--json", jp2, plain)
	c2 := readIngressOut(t, jp2)
	if c2.PerAttempt || c2.IngressRuns != 0 || len(c2.Samples) != 1 || c2.Samples[0].LSource != "sent" ||
		c2.Samples[0].LastAnyDel != nil {
		t.Errorf("no ingress.log: %+v", c2)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
