package winddown

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"koupa/rush-hour/record"
)

func TestParseIngressLog(t *testing.T) {
	lines := []string{
		`[2026-11-09T10:00:00.000Z] {"start_time":"2026-11-09T10:00:00.000Z","path":"/orders","method":"POST","status":201,"upstream_host":"10.0.0.5:8080","response_flags":"-","response_code_details":"via_upstream","upstream_attempts":1,"duration_ms":3,"attempt":"a1"}`,
		`{"start_time":"2026-11-09T10:00:00.000Z","path":"/orders","upstream_host":"-","response_flags":"UF,URX","upstream_attempts":0,"attempt":"a0"}`,
		`{"start_time":"2026-11-09T10:00:00.000Z","path":"/orders","upstream_host":"-","response_flags":"UH","upstream_attempts":0,"attempt":"aUH"}`,
		`{"start_time":"2026-11-09T10:00:00.000Z","path":"/orders","upstream_host":"10.0.0.6:8080","response_flags":"URX","upstream_attempts":"2","attempt":"a2"}`,
		`{"path":"/orders","upstream_attempts":1}`,
		`{"path":"/orders","upstream_attempts":1,"attempt":"-"}`,
		`not json at all`,
		`{"path":"/orders","upstream_attempts":1,"attempt":"dup"}`,
		`{"path":"/orders","upstream_attempts":1,"attempt":"dup"}`,
	}
	p := filepath.Join(t.TempDir(), "ingress.log")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ing := ReadIngressLog(p, map[string]string{"10.0.0.5": "uid-a"})
	if !ing.Available {
		t.Fatalf("not available: %s", ing.Err)
	}
	if len(ing.Recs) != 5 {
		t.Fatalf("recs %d: %v", len(ing.Recs), ing.Recs)
	}
	a1 := ing.Recs["a1"]
	if a1 == nil || !a1.Finished || a1.UpstreamAttempts == nil || *a1.UpstreamAttempts != 1 ||
		a1.UpstreamPod != "uid-a" || a1.IngressStart == nil {
		t.Errorf("a1 %+v", a1)
	}
	if r := ing.Recs["a0"]; r == nil || r.UpstreamAttempts != nil {
		t.Errorf("0 without UH/NR must be unknown: %+v", r)
	}
	if r := ing.Recs["aUH"]; r == nil || r.UpstreamAttempts == nil || *r.UpstreamAttempts != 0 {
		t.Errorf("UH must keep 0: %+v", r)
	}
	if r := ing.Recs["a2"]; r == nil || r.UpstreamAttempts == nil || *r.UpstreamAttempts != 2 {
		t.Errorf("a2 %+v", r)
	}
	if r := ing.Recs["dup"]; r == nil || r.UpstreamAttempts != nil {
		t.Errorf("duplicate lines must be unknown: %+v", r)
	}

	missing := ReadIngressLog(filepath.Join(t.TempDir(), "nope.log"), nil)
	if missing.Available || missing.Recs != nil {
		t.Errorf("missing file available: %+v", missing)
	}
}

func writeLog(t *testing.T, dir, uid string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, uid+".log"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAppLogCoverage(t *testing.T) {
	dir := t.TempDir()
	ts := "2026-11-09T10:00:00Z "
	startup := func(uid string, graceful bool) string {
		g := "false"
		if graceful {
			g = "true"
		}
		// The App logs test_mutation_* as numbers (rates), test_hooks as bool.
		return ts + `{"ts":"2026-11-09T10:00:00Z","event":"startup","pod_uid":"` + uid + `","graceful_shutdown":` + g +
			`,"test_hooks":false,"test_mutation_drop_ready":0,"test_mutation_duplicate_order":0}`
	}
	sigterm := func(uid string) string {
		return `{"ts":"2026-11-09T10:00:05Z","event":"sigterm","pod_uid":"` + uid + `"}`
	}
	exit := func(uid string) string {
		return `{"ts":"2026-11-09T10:00:06Z","event":"exit","pod_uid":"` + uid + `"}`
	}
	// step-0 pod: sigterm is the final marker.
	writeLog(t, dir, "p0", startup("p0", false), sigterm("p0"))
	// graceful pod with only sigterm: the exit marker is missing.
	writeLog(t, dir, "pg", startup("pg", true), sigterm("pg"))
	// graceful pod with exit.
	writeLog(t, dir, "pe", startup("pe", true), sigterm("pe"), exit("pe"))
	// running pod: no marker, meta says complete.
	writeLog(t, dir, "pr", startup("pr", true))
	if err := os.WriteFile(filepath.Join(dir, "pr.meta.json"), []byte(`{"pod_uid":"pr","pod_name":"koupa-r","complete":true,"exited_at":null,"term_start":null,"collect_start":"2026-11-09T09:59:00Z","collect_end":null,"stopped_at":"2026-11-09T10:01:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// no startup line.
	writeLog(t, dir, "pn", sigterm("pn"))
	// a broken JSON line.
	writeLog(t, dir, "pb", startup("pb", false), `{"ts":"2026-11-09T10:00:01Z","event":"request_rec`, sigterm("pb"))
	// request_end without request_received for the same attempt.
	writeLog(t, dir, "pc", startup("pc", false),
		`{"ts":"2026-11-09T10:00:01Z","event":"request_end","pod_uid":"pc","attempt":"x1","status":201,"write_err":"","flush_err":"","ctx_canceled":false,"tx_outcome":"committed"}`,
		sigterm("pc"))

	// a damaged line with no JSON at all (a lost record).
	writeLog(t, dir, "pd", startup("pd", false), `2026-11-09T10:00:01Z request_rec`, sigterm("pd"))
	// meta for another Pod: ignored, so the running Pod is not covered.
	writeLog(t, dir, "pm", startup("pm", true))
	if err := os.WriteFile(filepath.Join(dir, "pm.meta.json"), []byte(`{"pod_uid":"other","complete":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// final marker present, but the meta names another Pod / is broken:
	// the evidence for this Pod is unreliable.
	writeLog(t, dir, "px", startup("px", false), sigterm("px"))
	if err := os.WriteFile(filepath.Join(dir, "px.meta.json"), []byte(`{"pod_uid":"other","complete":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeLog(t, dir, "py", startup("py", false), sigterm("py"))
	if err := os.WriteFile(filepath.Join(dir, "py.meta.json"), []byte(`{"pod_uid":`), 0o644); err != nil {
		t.Fatal(err)
	}
	app := ReadAppLogs(dir)
	if !app.Available {
		t.Fatal(app.Err)
	}
	// The collector's meta is the only coverage authority: markers alone
	// (p0, pe, pc) no longer make a Pod covered.
	want := map[string]bool{"p0": false, "pg": false, "pe": false, "pr": true, "pn": false, "pb": false, "pc": false, "pd": false, "pm": false, "px": false, "py": false}
	for uid, w := range want {
		p := app.Pods[uid]
		if p == nil {
			t.Errorf("pod %s not read", uid)
			continue
		}
		if p.Covered != w {
			t.Errorf("pod %s covered %v, want %v", uid, p.Covered, w)
		}
	}
	if !app.Contradictory["x1"] {
		t.Error("request_end without request_received not contradictory")
	}
	if m := app.Pods["pm"]; m.Meta != nil || m.MetaErr == "" {
		t.Errorf("mismatched meta accepted: %+v", m)
	}
	if f := app.Pods["p0"].TestFlags; len(f) != 0 {
		t.Errorf("test flags %v for zero values", f)
	}
	writeLog(t, dir, "ph", `{"ts":"2026-11-09T10:00:00Z","event":"startup","pod_uid":"ph","graceful_shutdown":false,"test_hooks":true,"test_mutation_drop_ready":0.5,"test_mutation_duplicate_order":0}`)
	if f := ReadAppLogs(dir).Pods["ph"].TestFlags; strings.Join(f, ",") != "TEST_HOOKS,TEST_MUTATION drop_ready" {
		t.Errorf("test flags %v", f)
	}
	if got := app.Pods["pe"].SigtermAt; got == nil || !got.Equal(time.Date(2026, 11, 9, 10, 0, 5, 0, time.UTC)) {
		t.Errorf("sigterm at %v", got)
	}
}

func TestWaitGivesUpAtLimit(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC))
	start := time.Now()
	left := waitUntil(clk, 70*time.Second, time.Second, func(time.Time) int { return 3 })
	if left != 3 {
		t.Errorf("left %d", left)
	}
	if el := clk.Now().Sub(time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)); el < 70*time.Second || el > 71*time.Second {
		t.Errorf("fake time advanced %v, want 70s", el)
	}
	if time.Since(start) > time.Second {
		t.Errorf("real time %v: the fake clock was not used", time.Since(start))
	}
}

func TestWaitReturnsEarly(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	clk := newFakeClock(t0)
	left := waitUntil(clk, 70*time.Second, time.Second, func(now time.Time) int {
		if now.Sub(t0) >= 3*time.Second {
			return 0
		}
		return 1
	})
	if left != 0 || clk.Now().Sub(t0) != 3*time.Second {
		t.Errorf("left %d after %v", left, clk.Now().Sub(t0))
	}
}

func TestDefaultLimits(t *testing.T) {
	l := DefaultLimits(60 * time.Second)
	if l.Client != 10*time.Second || l.Server != 70*time.Second || l.Kitchen != 10*time.Second || l.Boards != 10*time.Second {
		t.Errorf("%+v", l)
	}
}

// fakeClock advances instantly: After moves time forward and fires at once.
type fakeClock struct{ now time.Time }

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

func TestCoverageComesFromMeta(t *testing.T) {
	dir := t.TempDir()
	start := `{"ts":"2026-11-09T10:00:00Z","event":"startup","pod_uid":"%s","graceful_shutdown":false}`
	sig := `{"ts":"2026-11-09T10:00:05Z","event":"sigterm","pod_uid":"%s"}`
	meta := func(uid, extra string) {
		b := `{"pod_uid":"` + uid + `","pod_name":"koupa-` + uid + `"` + extra + `}`
		if err := os.WriteFile(filepath.Join(dir, uid+".meta.json"), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, uid := range []string{"mk", "mw", "ms", "mn", "m0"} {
		writeLog(t, dir, uid, fmt.Sprintf(start, uid), fmt.Sprintf(sig, uid))
	}
	meta("mk", `,"complete":false,"collect_start":"2026-11-09T09:59:00Z","collect_end":"2026-11-09T10:00:10Z"`)
	meta("mw", `,"complete":true,"collect_start":"2026-11-09T10:00:00Z","collect_end":"2026-11-09T10:00:10Z"`)
	meta("ms", `,"complete":true,"collect_start":"2026-11-09T10:00:00Z","collect_end":null,"stopped_at":"2026-11-09T10:00:20Z"`)
	meta("mn", `,"complete":true,"collect_start":"2026-11-09T10:00:00Z","collect_end":null,"stopped_at":null`)
	app := ReadAppLogs(dir)
	at := func(sec int) time.Time {
		return time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC).Add(time.Duration(sec) * time.Second)
	}
	cases := []struct {
		uid  string
		sec  int
		want bool
	}{
		{"mk", 5, false}, // markers present, meta says incomplete
		{"m0", 5, false}, // markers present, no meta
		{"mw", 5, true}, {"mw", -1, false}, {"mw", 11, false},
		{"ms", 15, true}, {"ms", 21, false}, // collect_end null: the stop time
		{"mn", 5, false}, // no end at all
	}
	for _, c := range cases {
		if got := app.Pods[c.uid].CoveredAt(at(c.sec)); got != c.want {
			t.Errorf("%s at +%ds: covered %v, want %v", c.uid, c.sec, got, c.want)
		}
	}
	if app.Pods["mk"].Covered || app.Pods["m0"].Covered || !app.Pods["mw"].Covered {
		t.Error("pod-level Covered does not follow meta.complete")
	}
	// The attempt's own time decides LogCovered.
	ev := Evidence{App: app}
	in := record.Attempt{Attempt: "a1", PodUID: "mw", SentAt: at(2), RespondedAt: at(3)}
	if !assemble(in, ev, true, nil).LogCovered {
		t.Error("attempt inside the collection window not covered")
	}
	late := record.Attempt{Attempt: "a2", PodUID: "mw", SentAt: at(9), RespondedAt: at(12)}
	if assemble(late, ev, true, nil).LogCovered {
		t.Error("attempt ending after collect_end covered")
	}
}
