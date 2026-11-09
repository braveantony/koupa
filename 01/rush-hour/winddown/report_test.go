package winddown

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"koupa/rush-hour/record"
)

// loadCase reads testdata/<name>: attempts.jsonl, boards.jsonl, facts.json,
// and the optional app-logs/ and ingress.log (a missing one stays missing).
func loadCase(t *testing.T, name string, appLogs, ingress string) (Facts, Evidence) {
	t.Helper()
	dir := filepath.Join("testdata", name)
	f, err := ReadFacts(filepath.Join(dir, "facts.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev := LoadEvidence(Paths{
		Attempts: filepath.Join(dir, "attempts.jsonl"),
		Boards:   filepath.Join(dir, "boards.jsonl"),
		AppLogs:  appLogs,
		Ingress:  ingress,
	})
	return f, ev
}

func okCase(t *testing.T) (Facts, Evidence) {
	return loadCase(t, "ok", "testdata/ok/app-logs", "testdata/ok/ingress.log")
}

// mainLines returns the main section's "label：value" lines.
func mainLines(t *testing.T, text string) map[string]string {
	t.Helper()
	start := strings.Index(text, "【主要成績】")
	if start < 0 {
		t.Fatalf("no main section in:\n%s", text)
	}
	body := text[start:]
	if end := strings.Index(body, "【診斷資訊】"); end >= 0 {
		body = body[:end]
	}
	out := map[string]string{}
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(l)
		if k, v, ok := strings.Cut(l, "："); ok && v != "" {
			out[k] = v
		}
	}
	return out
}

func TestReportMainSection(t *testing.T) {
	f, ev := okCase(t)
	sc := Evaluate(f, ev)
	if sc.Invalid {
		t.Fatalf("invalid: %v", sc.InvalidReasons)
	}
	if sc.HasUnknown {
		t.Fatalf("has_unknown with complete evidence; unsettled %+v", sc.Diag.Unsettled)
	}
	text := sc.Text(false)
	if strings.Contains(text, "【診斷資訊】") {
		t.Error("diagnostics shown without --verbose")
	}
	got := mainLines(t, text)
	want := map[string]string{
		"訂單":                 "3 張",
		"第一次送出沒有成功回應":        "1 張",
		"重試後仍沒有成功回應":         "0 張",
		"重複訂單（client_ref 數）": "1",
		"多出的訂單張數":            "1 張",
		"board-1":            "0",
		"board-2":            "1",
		"board-3":            "0",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q\n%s", k, got[k], v, text)
		}
	}
	if sc.ExitCode() != 0 {
		t.Errorf("exit %d", sc.ExitCode())
	}
}

func TestReportVerboseSection(t *testing.T) {
	f, ev := okCase(t)
	text := Evaluate(f, ev).Text(true)
	for _, s := range []string{
		"【診斷資訊】",
		"r1-a2-t1",
		"客人收到錯誤回應",
		"收到 shutdown 後斷線：1",
		"沒收到 shutdown 就斷線：1",
		"SIGTERM 之後才到的請求",
		"uid-a：1",
		"未確定的項目：無",
		"時間線",
		"開始送單",
		"主機資源",
		"CPU",
	} {
		if !strings.Contains(text, s) {
			t.Errorf("verbose section lacks %q\n%s", s, text)
		}
	}
	// board-3's final disconnect came from stopping the boards: not counted.
	if strings.Count(text, "沒收到 shutdown 就斷線：1") != 1 {
		t.Errorf("run-end disconnect counted\n%s", text)
	}
}

func TestReportUnknownNeverZero(t *testing.T) {
	// No ingress log, no App logs, one client in flight at the client
	// limit, board-2 not caught up: every main score has unknowns.
	f, ev := loadCase(t, "unknown", "", "testdata/unknown/ingress-missing.log")
	sc := Evaluate(f, ev)
	if sc.Invalid {
		t.Fatalf("invalid: %v", sc.InvalidReasons)
	}
	if !sc.HasUnknown {
		t.Fatal("has_unknown false")
	}
	text := sc.Text(true)
	if !strings.Contains(text, "入口紀錄取不到") {
		t.Errorf("verbose lacks 入口紀錄取不到\n%s", text)
	}
	bareZero := regexp.MustCompile(`^0(\s|$)`)
	lines := mainLines(t, text)
	if len(lines) < 8 {
		t.Fatalf("main lines %v", lines)
	}
	for k, v := range lines {
		if bareZero.MatchString(v) {
			t.Errorf("%s printed as bare 0: %q", k, v)
		}
	}
	for _, k := range []string{"第一次送出沒有成功回應", "重試後仍沒有成功回應", "重複訂單（client_ref 數）", "多出的訂單張數", "board-1", "board-2", "board-3"} {
		if !strings.Contains(lines[k], "未知") {
			t.Errorf("%s = %q, want an unknown part", k, lines[k])
		}
	}
	if !strings.Contains(lines["重複訂單（client_ref 數）"], "伺服器端結果未知，可能增加") {
		t.Errorf("duplicates wording: %q", lines["重複訂單（client_ref 數）"])
	}
	// JSON exposes the same.
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	var j struct {
		HasUnknown bool `json:"has_unknown"`
		Invalid    bool `json:"invalid"`
		Items      struct {
			FirstTryFailed Item            `json:"first_try_failed"`
			Missing        map[string]Item `json:"missing"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &j); err != nil {
		t.Fatal(err)
	}
	if !j.HasUnknown || j.Invalid || j.Items.FirstTryFailed.Unknown != 1 || j.Items.Missing["board-2"].Unknown != 2 {
		t.Errorf("json %s", b)
	}
}

func TestInvalidWhenBoardHasNoStart(t *testing.T) {
	f, ev := okCase(t)
	var kept []record.BoardEvent
	for _, e := range ev.Boards {
		if !(e.Board == "board-3" && e.Kind == record.BoardHello) {
			kept = append(kept, e)
		}
	}
	ev.Boards = kept
	sc := Evaluate(f, ev)
	if !sc.Invalid || sc.ExitCode() != 2 {
		t.Fatalf("invalid %v exit %d", sc.Invalid, sc.ExitCode())
	}
	dir := t.TempDir()
	if err := WriteScorecard(dir, sc, false); err != nil {
		t.Fatal(err)
	}
	txt, err := os.ReadFile(filepath.Join(dir, "scorecard.txt"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(txt), "\n")
	if !strings.HasPrefix(first, "本次量測無效") || !strings.Contains(first, "board-3") {
		t.Errorf("first line %q", first)
	}
	if strings.Contains(string(txt), "【主要成績】") {
		t.Error("invalid run still prints main scores")
	}
	var j map[string]any
	b, _ := os.ReadFile(filepath.Join(dir, "scorecard.json"))
	if err := json.Unmarshal(b, &j); err != nil {
		t.Fatal(err)
	}
	if j["invalid"] != true || j["invalid_reason"] == "" {
		t.Errorf("json invalid=%v reason=%v", j["invalid"], j["invalid_reason"])
	}
}

func TestInvalidWhenAuditFailed(t *testing.T) {
	f, ev := okCase(t)
	f.Audit.Cursor = nil
	f.Audit.Errors = append(f.Audit.Errors, "cursor: 503")
	if sc := Evaluate(f, ev); !sc.Invalid {
		t.Fatal("audit failure not invalid")
	}
}

func TestInvalidWhenEvidenceWriteFailed(t *testing.T) {
	f, ev := okCase(t)
	f.Invalid = append(f.Invalid, "attempts.jsonl 寫入失敗：disk full")
	sc := Evaluate(f, ev)
	if !sc.Invalid || !strings.HasPrefix(sc.Text(false), "本次量測無效") {
		t.Fatalf("write failure not invalid:\n%s", sc.Text(false))
	}
}

func TestNotIncreasingIncompleteBoard(t *testing.T) {
	f, ev := okCase(t)
	ev.BoardsBad = 1 // an unreadable line: no board record is complete
	text := Evaluate(f, ev).Text(true)
	if !strings.Contains(text, "未發現違反（紀錄不完整）") {
		t.Errorf("incomplete board wording missing\n%s", text)
	}
}

func TestNoIngressIsProminent(t *testing.T) {
	f, ev := loadCase(t, "ok", "testdata/ok/app-logs", "")
	f.NoIngress = true
	sc := Evaluate(f, ev)
	if sc.HasUnknown || sc.Invalid {
		t.Fatalf("unknown %v invalid %v %+v", sc.HasUnknown, sc.Invalid, sc.Diag.Unsettled)
	}
	head := strings.SplitN(sc.Text(false), "\n", 4)
	if !strings.Contains(strings.Join(head[:3], "\n"), "--no-ingress") {
		t.Errorf("--no-ingress not near the top:\n%s", sc.Text(false))
	}
}

func writeCard(t *testing.T, sc Scorecard) string {
	t.Helper()
	dir := t.TempDir()
	if err := WriteScorecard(dir, sc, false); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSummarizeMedianRange(t *testing.T) {
	mk := func(run string, orders, first int, unknownFirst int) Scorecard {
		return Scorecard{Run: run, Items: Items{
			Orders:         Item{Confirmed: orders},
			FirstTryFailed: Item{Confirmed: first, Unknown: unknownFirst},
			Missing:        map[string]Item{"board-1": {}},
		}}
	}
	dirs := []string{
		writeCard(t, mk("r1", 10, 1, 0)),
		writeCard(t, mk("r2", 30, 5, 0)),
		writeCard(t, mk("r3", 20, 2, 0)),
		writeCard(t, mk("r4", 40, 0, 3)),
	}
	inv := mk("r5", 99, 99, 0)
	inv.Invalid, inv.InvalidReason = true, "看板 board-1 沒有起點"
	dirs = append(dirs, writeCard(t, inv))
	out, err := Summarize(dirs)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"r1", "r2", "r3", "r4",
		"訂單：中位數 25，範圍 10～40",
		"第一次送出沒有成功回應：中位數 2，範圍 1～5（另有 1 次含未知，未列入）",
		"已確認 0；另有 3 張未知",
		"r5：本次量測無效",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("summary lacks %q\n%s", s, out)
		}
	}
	if strings.Contains(out, "99") {
		t.Errorf("invalid run counted\n%s", out)
	}
}

func TestReportMixedUnknownBanner(t *testing.T) {
	// One order attempt retried at the ingress (unsettled) and board-2 not
	// caught up; everything else is decided.
	f, ev := okCase(t)
	two := 2
	ev.Ingress.Recs["r1-a1-t1"].UpstreamAttempts = &two
	f.CaughtUp["board-2"] = false
	sc := Evaluate(f, ev)
	if sc.Invalid || !sc.HasUnknown {
		t.Fatalf("invalid %v unknown %v", sc.Invalid, sc.HasUnknown)
	}
	text := sc.Text(false)
	lines := strings.Split(text, "\n")
	if lines[1] != "本次有 2 項結果無法確定，見診斷資訊；未標示未知的項目不受影響" {
		t.Errorf("banner line %q\n%s", lines[1], text)
	}
	got := mainLines(t, text)
	want := map[string]string{
		"訂單":                 "3 張",
		"第一次送出沒有成功回應":        "1 張",
		"重試後仍沒有成功回應":         "0 張",
		"重複訂單（client_ref 數）": "已確認 1；另有 1 筆伺服器端結果未知，可能增加",
		"多出的訂單張數":            "已確認 1；另有 1 筆伺服器端結果未知，可能增加",
		"board-1":            "0",
		"board-2":            "已確認 0；另有 1 個叫號未知",
		"board-3":            "0",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	// Reasons are shown in Traditional Chinese; JSON keeps the code.
	v := sc.Text(true)
	if !strings.Contains(v, "入口重試過上游") || strings.Contains(v, "ingress retried upstream") {
		t.Errorf("reason not translated\n%s", v)
	}
	if sc.Diag.Unsettled[0].Reason != "ingress retried upstream; App cannot tell the tries apart" {
		t.Errorf("json reason %q", sc.Diag.Unsettled[0].Reason)
	}
}

func TestNoBannerWhenAllKnown(t *testing.T) {
	f, ev := okCase(t)
	if strings.Contains(Evaluate(f, ev).Text(false), "無法確定") {
		t.Error("banner without unknowns")
	}
}

func TestInFlightWithoutRecordIsUnsettled(t *testing.T) {
	// r1-c9 was cancelled before its first attempt was recorded: it may
	// still create an order, so duplicates cannot be a confirmed 0/1.
	f, ev := okCase(t)
	f.ClientInFlight = []string{"r1-c9"}
	at := f.EvaluatedAt
	f.ClientCancelAt = &at
	sc := Evaluate(f, ev)
	if sc.Items.Orders != (Item{3, 1}) || sc.Items.DuplicateRefs != (Item{1, 1}) || sc.Items.DuplicateExtra != (Item{1, 1}) {
		t.Errorf("items %+v", sc.Items)
	}
}

func TestEveryUnsettledReasonTranslated(t *testing.T) {
	src, err := os.ReadFile("../verdict/settle.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`unsettled\("([^"]+)"`)
	ms := re.FindAllStringSubmatch(string(src), -1)
	if len(ms) < 10 {
		t.Fatalf("found only %d reasons", len(ms))
	}
	for _, m := range ms {
		if reasonText(m[1]) == m[1] {
			t.Errorf("reason %q has no Traditional Chinese text", m[1])
		}
	}
	if !strings.Contains(reasonText("request ended with unrecognised outcome weird"), "weird") {
		t.Error("prefix reason")
	}
}
