package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"koupa/rush-hour/record"
)

const run = "r1"

func ip(n int) *int { return &n }

func att(id string, try int, status *int, errKind string) record.Attempt {
	ref := strings.Split(id, "-t")[0]
	return record.Attempt{Run: run, Kind: record.KindOrder, ClientRef: ref, Attempt: id, Try: try,
		Status: status, ErrKind: errKind, FullResponse: status != nil && errKind == ""}
}

func line(attempt, method, path string, status int, flags string, upstream int, start string, durMs int) string {
	a := `"` + attempt + `"`
	if attempt == "" {
		a = "null"
	}
	return `1794214439.9 {"attempt":` + a + `,"duration_ms":` + itoa(durMs) + `,"method":"` + method +
		`","path":"` + path + `","response_code_details":"x","response_flags":"` + flags +
		`","start_time":"` + start + `","status":` + itoa(status) + `,"upstream_attempts":` + itoa(upstream) +
		`,"upstream_host":"10.0.0.1:8080"}`
}

func itoa(n int) string { return strconv.Itoa(n) }

const t0 = "2026-11-09T08:53:53.000Z"

func fixture() ([]record.Attempt, []IngressLine) {
	atts := []record.Attempt{
		att("r1-a1-t1", 1, ip(201), ""),    // matched
		att("r1-a2-t1", 1, nil, "timeout"), // client timeout, ingress DC
		att("r1-a2-t2", 2, ip(201), ""),    // retry succeeded
		att("r1-a3-t1", 1, nil, "timeout"), // client timeout, ingress answered late
		att("r1-a3-t2", 2, nil, "refused"), // never reached the ingress
		att("r1-a4-t1", 1, ip(503), ""),    // RH has a status but no ingress line
		att("r1-a4-t2", 2, ip(201), ""),    // matched
		att("r1-a5-t1", 1, ip(201), ""),    // ingress retried internally
		att("r1-a6-t1", 1, ip(201), ""),    // status mismatch
		att("r1-a7-t1", 1, ip(503), ""),    // matched failure
		att("r1-a7-t2", 2, ip(503), ""),    // matched failure after retry
	}
	// A kitchen attempt (not an order) must stay out of the /orders scope.
	atts = append(atts, record.Attempt{Run: run, Kind: record.KindReady, Attempt: "r1-k-a1-t1", Try: 1, Status: ip(200), FullResponse: true})
	text := strings.Join([]string{
		line("r1-a1-t1", "POST", "/orders", 201, "-", 1, t0, 3),
		line("r1-a2-t1", "POST", "/orders", 0, "DC", 1, t0, 5000),
		line("r1-a2-t2", "POST", "/orders", 201, "-", 1, t0, 3),
		line("r1-a3-t1", "POST", "/orders", 201, "-", 1, t0, 6000),
		line("r1-a4-t2", "POST", "/orders", 201, "-", 1, t0, 3),
		line("r1-a5-t1", "POST", "/orders", 201, "-", 2, t0, 3),
		line("r1-a6-t1", "POST", "/orders", 503, "UF", 1, t0, 3),
		line("r1-a7-t1", "POST", "/orders", 503, "UF", 1, t0, 3),
		line("r1-a7-t2", "POST", "/orders", 503, "UC", 1, t0, 3),
		line("r1-a9-t1", "POST", "/orders", 201, "-", 1, t0, 3),           // no Rush Hour record
		line("other-a1-t1", "POST", "/orders", 201, "-", 1, t0, 3),        // another run
		line("", "POST", "/orders", 201, "-", 1, t0, 3),                   // no attempt header (browser)
		line("r1-k-a1-t1", "POST", "/orders/5/ready", 200, "-", 1, t0, 3), // other path
		line("", "GET", "/api/orders?status=pending", 200, "-", 1, t0, 3),
		"garbage line without json",
	}, "\n")
	lines, bad := ParseIngress(strings.NewReader(text))
	if bad != 0 {
		panic("fixture lines must parse")
	}
	return atts, lines
}

func TestReconcileAttempts(t *testing.T) {
	atts, lines := fixture()
	r := ReconcileAttempts(run, atts, lines, nil)
	check := func(name string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	check("RushHour (order attempts)", r.RushHour, 11)
	check("Ingress (/orders lines of the run)", r.Ingress, 10)
	check("Matched", r.Matched, 9)
	check("SameStatus", r.SameStatus, 6)
	check("ClientTimeout", r.Diff[CatClientTimeout], 2)            // a2-t1 (DC), a3-t1 (answered late)
	check("NotViaIngress", r.Diff[CatNotViaIngress], 0)            // no independent evidence of a bypass
	check("IngressRetry", r.Diff[CatIngressRetry], 1)              // a5-t1 upstream_attempts 2
	check("OtherTraffic", r.Diff[CatOther], 4)                     // other run, no header, kitchen, GET
	check("Unexplained", r.Diff[CatUnexplained], 4)                // a3-t2 and a4-t1 no line, a9 no record, a6 mismatch
	check("FirstTryFailedRH", r.FirstTryFailedRH, 4)               // a2 a3 a4 a7
	check("FailedAfterRetryRH", r.FailedAfterRetryRH, 2)           // a3 a7
	check("FirstTryFailedIngress", r.FirstTryFailedIngress, 4)     // a2 (0) a4 (no line) a6 (503) a7
	check("FailedAfterRetryIngress", r.FailedAfterRetryIngress, 3) // a3 (no line) a6 a7
	if len(r.UnexplainedList) != 4 {
		t.Errorf("UnexplainedList = %v, want 4 entries", r.UnexplainedList)
	}
}

func TestParseStatsSumsAgents(t *testing.T) {
	text := `# collected_at=100.5 node=w1 pod=cilium-a
envoy_cluster_upstream_rq_total{envoy_cluster_name="koupa/x"} 10
envoy_cluster_upstream_rq_xx{envoy_response_code_class="2",envoy_cluster_name="koupa/x"} 7
# collected_at=101.0 node=w2 pod=cilium-b
envoy_cluster_upstream_rq_total{envoy_cluster_name="koupa/x"} 5
`
	s, err := ParseStats(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if s.Agents != 2 || s.Get("envoy_cluster_upstream_rq_total", "") != 15 {
		t.Errorf("agents %d total %v", s.Agents, s.Get("envoy_cluster_upstream_rq_total", ""))
	}
	if s.Get("envoy_cluster_upstream_rq_xx", `envoy_response_code_class="2"`) != 7 {
		t.Errorf("xx 2 = %v", s.Get("envoy_cluster_upstream_rq_xx", `envoy_response_code_class="2"`))
	}
	if s.Get("envoy_cluster_upstream_rq_retry", "") != 0 {
		t.Error("absent counter must read 0")
	}
	if s.First.Unix() != 100 || s.Last.Unix() != 101 {
		t.Errorf("window %v %v", s.First, s.Last)
	}
	if _, err := ParseStats(strings.NewReader("")); err == nil {
		t.Error("no agent: want error")
	}
}

func TestCompareStats(t *testing.T) {
	start := `# collected_at=1794214400 node=w1 pod=a
envoy_cluster_upstream_rq_total{envoy_cluster_name="koupa/x"} 100
envoy_cluster_upstream_rq_completed{envoy_cluster_name="koupa/x"} 100
envoy_cluster_upstream_rq_pending_failure_eject{envoy_cluster_name="koupa/x"} 0
envoy_cluster_upstream_rq_cancelled{envoy_cluster_name="koupa/x"} 0
envoy_cluster_upstream_rq_xx{envoy_response_code_class="2",envoy_cluster_name="koupa/x"} 100
`
	// Window delta: 6 completed = 4 requests sent upstream (one retried
	// inside the ingress: +1) + 1 connection failure.
	end := `# collected_at=1794214500 node=w1 pod=a
envoy_cluster_upstream_rq_total{envoy_cluster_name="koupa/x"} 105
envoy_cluster_upstream_rq_completed{envoy_cluster_name="koupa/x"} 106
envoy_cluster_upstream_rq_pending_failure_eject{envoy_cluster_name="koupa/x"} 1
envoy_cluster_upstream_rq_cancelled{envoy_cluster_name="koupa/x"} 1
envoy_cluster_upstream_rq_xx{envoy_response_code_class="2",envoy_cluster_name="koupa/x"} 105
envoy_cluster_upstream_rq_xx{envoy_response_code_class="5",envoy_cluster_name="koupa/x"} 1
`
	in := "2026-11-09T08:53:30.000Z" // inside the window (08:53:20 .. 08:55:00 UTC)
	before := "2026-11-09T08:53:10.000Z"
	text := strings.Join([]string{
		line("r1-a1-t1", "POST", "/orders", 201, "-", 1, in, 3),
		line("r1-a2-t1", "POST", "/orders", 201, "-", 2, in, 3),
		line("r1-a3-t1", "POST", "/orders", 503, "UF", 1, in, 3),
		line("r1-a4-t1", "POST", "/orders", 0, "DC", 1, in, 3),
		line("", "GET", "/api/orders", 200, "-", 1, in, 3),
		line("r0-a1-t1", "POST", "/orders", 201, "-", 1, before, 3), // before the window
	}, "\n")
	lines, _ := ParseIngress(strings.NewReader(text))
	s0, _ := ParseStats(strings.NewReader(start))
	s1, _ := ParseStats(strings.NewReader(end))
	c := CompareStats(s0, s1, lines)
	if c.OutsideWindow != 1 || c.InWindow != 5 {
		t.Fatalf("window: in %d outside %d", c.InWindow, c.OutsideWindow)
	}
	row := func(key string) StatsRow {
		for _, r := range c.Rows {
			if r.Key == key {
				return r
			}
		}
		t.Fatalf("no row %s", key)
		return StatsRow{}
	}
	// completed (6) = 5 lines + 1 ingress-internal retry.
	if r := row("completed"); r.Envoy != 6 || r.Log != 5 || r.Unexplained != 0 || r.Explained[CatIngressRetry] != 1 {
		t.Errorf("completed row %+v", r)
	}
	// rq_total (5) = completed - connection failures (UF lines, pending_failure_eject).
	if r := row("rq_total"); r.Envoy != 5 || r.Log != 4 || r.Unexplained != 0 || r.Explained[CatIngressRetry] != 1 {
		t.Errorf("rq_total row %+v", r)
	}
	if r := row("cancelled"); r.Envoy != 1 || r.Log != 1 {
		t.Errorf("cancelled row %+v", r)
	}
	// 2xx: 5 in Envoy vs 3 lines; one may be the internal retry's extra
	// upstream answer, the other stays visible as unexplained.
	if r := row("2xx"); r.Envoy != 5 || r.Log != 3 || r.Retry != 1 || r.Unexplained != 1 {
		t.Errorf("2xx row %+v", r)
	}
	// /orders share of the cluster-wide counter: completed minus the other
	// paths' lines (6 - 1) vs /orders lines (4) + their internal retry (1).
	if r := row("orders_share"); r.Envoy != 5 || r.Log != 4 || r.Retry != 1 || r.Unexplained != 0 {
		t.Errorf("orders_share row %+v", r)
	}
}

func TestRunWithoutIngressLog(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "attempts.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := runMain([]string{dir}, &out, &errb); code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
	if !strings.Contains(out.String(), "入口紀錄取不到") {
		t.Errorf("output %q", out.String())
	}
}

func TestRunRendersTables(t *testing.T) {
	dir := t.TempDir()
	atts, _ := fixture()
	var buf bytes.Buffer
	w := record.NewWriter(&buf)
	for _, a := range atts {
		_ = w.Write(a)
	}
	_ = os.WriteFile(filepath.Join(dir, "attempts.jsonl"), buf.Bytes(), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "params.json"), []byte(`{"run_id":"r1"}`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "ingress.log"), []byte(line("r1-a1-t1", "POST", "/orders", 201, "-", 1, t0, 3)+"\n"), 0o644)
	stats := "# collected_at=1794214400 node=w1 pod=a\nenvoy_cluster_upstream_rq_total{envoy_cluster_name=\"koupa/x\"} 1\n"
	_ = os.WriteFile(filepath.Join(dir, "envoy-stats-start.txt"), []byte(stats), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "envoy-stats-end.txt"), []byte(stats), 0o644)
	var out, errb bytes.Buffer
	if code := runMain([]string{"--json", filepath.Join(dir, "r.json"), dir}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, want := range []string{"客人端逾時", "入口內部重試", "其他路徑或其他流量", "沒經過入口", "對不上", "Envoy 統計"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "r.json")); err != nil {
		t.Error("no json written")
	}
}

const missingRestart = `{"source":"hubble","reason":"x","at":"2026-11-09T13:14:05.999Z"}
{"source":"ingress","reason":"cilium-envoy-6rzcp 的 access log 串流中斷（rc=1），從 2026-11-09T13:14:05Z 重新讀取；同一秒內可能重複或缺行","at":"2026-11-09T13:14:05.999167822Z"}
`

func stamped(stamp, l string) string { return stamp + l[strings.IndexByte(l, ' '):] }

// No line is ever dropped. A line identical to an earlier one with a
// collector restart (missing.jsonl) between the two receive stamps is
// flagged "possible reread"; identical lines without a restart between
// them are not flagged (two requests can look alike).
func TestMarkRereads(t *testing.T) {
	in := "2026-11-09T13:14:05.300Z"
	out := "2026-11-09T13:13:00.000Z"
	text := strings.Join([]string{
		stamped("1794230045.31", line("r1-a1-t1", "POST", "/orders", 201, "-", 1, in, 3)),
		stamped("1794230046.10", line("r1-a1-t1", "POST", "/orders", 201, "-", 1, in, 3)), // after the restart
		stamped("1794229980.01", line("", "GET", "/api/orders", 200, "-", 1, out, 3)),
		stamped("1794229980.02", line("", "GET", "/api/orders", 200, "-", 1, out, 3)), // look-alike, no restart between
	}, "\n")
	lines, _ := ParseIngress(strings.NewReader(text))
	rr := ParseRereads(strings.NewReader(missingRestart))
	if len(rr) != 1 || rr[0].From.Format(time.RFC3339) != "2026-11-09T13:14:05Z" {
		t.Fatalf("rereads %+v", rr)
	}
	got := MarkRereads(lines, rr)
	if len(got) != 4 {
		t.Fatalf("%d lines, want all 4 kept", len(got))
	}
	var flags []bool
	for _, l := range got {
		flags = append(flags, l.PossibleReread)
	}
	if fmt.Sprint(flags) != "[false true false false]" {
		t.Errorf("possible reread flags %v, want [false true false false]", flags)
	}
	if n := len(MarkRereads(lines, nil)); n != 4 {
		t.Errorf("without restarts: %d lines", n)
	}
}

// A second /orders line for the same attempt id: "possible reread" (shown
// with its id, not matched again) when a restart lies between the two
// receive stamps, otherwise a real extra entry (unexplained).
func TestReconcileSecondLineForAttempt(t *testing.T) {
	text := strings.Join([]string{
		stamped("1794230045.31", line("r1-a1-t1", "POST", "/orders", 201, "-", 1, t0, 3)),
		stamped("1794230046.10", line("r1-a1-t1", "POST", "/orders", 201, "-", 1, t0, 3)), // restart between
		stamped("1794230050.00", line("r1-a2-t1", "POST", "/orders", 201, "-", 1, t0, 3)),
		stamped("1794230050.50", line("r1-a2-t1", "POST", "/orders", 503, "UF", 1, t0, 3)), // no restart between
	}, "\n")
	lines, _ := ParseIngress(strings.NewReader(text))
	lines = MarkRereads(lines, ParseRereads(strings.NewReader(missingRestart)))
	atts := []record.Attempt{att("r1-a1-t1", 1, ip(201), ""), att("r1-a2-t1", 1, ip(201), "")}
	r := ReconcileAttempts(run, atts, lines, ParseRereads(strings.NewReader(missingRestart)))
	if r.Matched != 2 || r.SameStatus != 2 {
		t.Errorf("matched %d same %d, want 2 2", r.Matched, r.SameStatus)
	}
	if r.PossibleReread != 1 || fmt.Sprint(r.PossibleRereadIDs) != "[r1-a1-t1]" {
		t.Errorf("possible reread %d %v, want 1 [r1-a1-t1]", r.PossibleReread, r.PossibleRereadIDs)
	}
	if r.Diff[CatUnexplained] != 1 || len(r.UnexplainedList) != 1 || !strings.Contains(r.UnexplainedList[0], "r1-a2-t1") {
		t.Errorf("unexplained %d %v, want the second r1-a2-t1 line", r.Diff[CatUnexplained], r.UnexplainedList)
	}
}

// Possible-reread lines are left out of the statistics comparison but
// counted and shown.
func TestCompareStatsSkipsPossibleReread(t *testing.T) {
	st := "# collected_at=1794214400 node=w1 pod=a\nenvoy_cluster_upstream_rq_completed{envoy_cluster_name=\"koupa/x\"} 0\n"
	en := "# collected_at=1794214500 node=w1 pod=a\nenvoy_cluster_upstream_rq_completed{envoy_cluster_name=\"koupa/x\"} 1\n"
	s0, _ := ParseStats(strings.NewReader(st))
	s1, _ := ParseStats(strings.NewReader(en))
	in := "2026-11-09T08:53:30.000Z"
	lines, _ := ParseIngress(strings.NewReader(line("r1-a1-t1", "POST", "/orders", 201, "-", 1, in, 3) + "\n" + line("r1-a1-t1", "POST", "/orders", 201, "-", 1, in, 3)))
	lines[1].PossibleReread = true
	c := CompareStats(s0, s1, lines)
	if c.PossibleReread != 1 || c.InWindow != 1 {
		t.Errorf("possible reread %d, in window %d; want 1, 1", c.PossibleReread, c.InWindow)
	}
}

// Malformed attempt lines are counted and shown, not silently skipped.
func TestRunShowsParseFailures(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "attempts.jsonl"), []byte("{bad\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "params.json"), []byte(`{"run_id":"r1"}`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "ingress.log"), []byte(line("r1-a1-t1", "POST", "/orders", 201, "-", 1, t0, 3)+"\n1.0 {not json\n"), 0o644)
	var out, errb bytes.Buffer
	runMain([]string{dir}, &out, &errb)
	if !strings.Contains(out.String(), "attempts.jsonl 無法解析 1 行") || !strings.Contains(out.String(), "ingress.log 無法解析 1 行") {
		t.Errorf("output lacks parse-failure counts:\n%s", out.String())
	}
}
