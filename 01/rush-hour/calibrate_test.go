package main

import (
	"bytes"
	"encoding/json"
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
		rec := record.Attempt{Run: filepath.Base(dir), Kind: record.KindProbe, Try: 1,
			ScheduledAt: calibT0.Add(at), SentAt: calibT0.Add(at)}
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
