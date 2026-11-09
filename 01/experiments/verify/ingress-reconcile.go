// Command ingress-reconcile compares Rush Hour's attempt records with the
// ingress access log and the Envoy cluster statistics of one measurement
// (plan A14 "和入口的紀錄對帳", design 5.6).
//
//	go run ./experiments/verify/ingress-reconcile.go [--json FILE] EVIDENCE_DIR
//
// EVIDENCE_DIR is a run directory written by measure.sh: attempts.jsonl,
// ingress.log (collect-timeline.sh), envoy-stats-start.txt and
// envoy-stats-end.txt, params.json (run_id).
//
// Scope of the per-attempt table: POST /orders lines whose X-Koupa-Attempt
// belongs to the run (final responses only: Envoy logs one line per
// downstream request, its internal retries show up as upstream_attempts > 1)
// against Rush Hour's order attempts, matched by attempt id. The Envoy
// counters are cluster-wide with no path dimension, so they are compared
// with every access-log line in the statistics window, and the /orders share
// is derived by taking the other paths off. Differences are put in four
// categories (client-side timeout, ingress-internal retry, other paths or
// other traffic, not via the ingress); whatever none of them explains is
// kept as 對不上 and listed.
//
// Exit: 0 tables written; 3 no ingress log (prints 入口紀錄取不到); 2 usage
// or unreadable evidence.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"koupa/rush-hour/record"
)

func main() { os.Exit(runMain(os.Args[1:], os.Stdout, os.Stderr)) }

// Difference categories (A14).
const (
	CatClientTimeout = "客人端逾時"
	CatIngressRetry  = "入口內部重試"
	CatOther         = "其他路徑或其他流量"
	CatNotViaIngress = "沒經過入口"
	CatUnexplained   = "對不上"
)

// IngressLine is one access-log line (contract: rush-hour/winddown
// IngressLog, Task 14 keys).
type IngressLine struct {
	StartTime           string          `json:"start_time"`
	Path                string          `json:"path"`
	Method              string          `json:"method"`
	Status              json.RawMessage `json:"status"`
	UpstreamHost        string          `json:"upstream_host"`
	ResponseFlags       string          `json:"response_flags"`
	ResponseCodeDetails string          `json:"response_code_details"`
	UpstreamAttempts    json.RawMessage `json:"upstream_attempts"`
	DurationMs          json.RawMessage `json:"duration_ms"`
	Attempt             *string         `json:"attempt"`
}

func rawInt(r json.RawMessage) (int, bool) {
	var n json.Number
	if json.Unmarshal(r, &n) == nil {
		if v, err := strconv.Atoi(n.String()); err == nil {
			return v, true
		}
	}
	var s string
	if json.Unmarshal(r, &s) == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return v, true
		}
	}
	return 0, false
}

func (l IngressLine) status() int     { n, _ := rawInt(l.Status); return n }
func (l IngressLine) durationMs() int { n, _ := rawInt(l.DurationMs); return n }
func (l IngressLine) attempt() string {
	if l.Attempt == nil || *l.Attempt == "-" {
		return ""
	}
	return *l.Attempt
}

// upstreamAttempts: at least 1 for a line that reached the upstream stage.
func (l IngressLine) upstreamAttempts() int {
	n, ok := rawInt(l.UpstreamAttempts)
	if !ok {
		return 0
	}
	return n
}
func (l IngressLine) hasFlag(f string) bool {
	for _, x := range strings.Split(l.ResponseFlags, ",") {
		if strings.TrimSpace(x) == f {
			return true
		}
	}
	return false
}
func (l IngressLine) isOrders() bool { return l.Method == "POST" && l.Path == "/orders" }
func (l IngressLine) start() (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, l.StartTime)
	return t, err == nil
}

// ParseIngress reads access-log lines (any prefix before the first '{').
// Lines without '{' are skipped; bad counts lines that did not parse.
func ParseIngress(r io.Reader) ([]IngressLine, int) {
	var out []IngressLine
	bad := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		b := sc.Bytes()
		i := bytes.IndexByte(b, '{')
		if i < 0 {
			continue
		}
		var l IngressLine
		if json.Unmarshal(b[i:], &l) != nil {
			bad++
			continue
		}
		out = append(out, l)
	}
	return out, bad
}

// DedupeLines drops lines identical to an earlier one (all fields): a
// collector that restarts an access-log stream re-reads from the same
// second (collect-timeline.sh records that in missing.jsonl). Returns the
// lines kept and how many were dropped.
func DedupeLines(lines []IngressLine) ([]IngressLine, int) {
	seen := map[string]bool{}
	out := make([]IngressLine, 0, len(lines))
	for _, l := range lines {
		b, _ := json.Marshal(l)
		if seen[string(b)] {
			continue
		}
		seen[string(b)] = true
		out = append(out, l)
	}
	return out, len(lines) - len(out)
}

// ---------------------------------------------------------------- per attempt

// AttemptTable is the /orders per-attempt reconciliation.
type AttemptTable struct {
	RushHour   int // Rush Hour order attempts of the run
	Ingress    int // POST /orders lines with an attempt id of the run
	Matched    int // attempt id in both
	SameStatus int // matched, Rush Hour got the complete response with the logged status
	DupLines   int // attempt ids logged more than once (collector re-read)
	// Diff counts per category. CatIngressRetry is informational (the line
	// matched; the ingress sent it upstream more than once).
	Diff            map[string]int
	UnexplainedList []string
	// "送出沒有成功回應" seen by each side, per client_ref.
	FirstTryFailedRH        int
	FailedAfterRetryRH      int
	FirstTryFailedIngress   int // the first attempt's line is missing or not 2xx
	FailedAfterRetryIngress int // the last attempt's line is missing or not 2xx
}

func ok2xx(s int) bool { return s/100 == 2 }

func rhOK(a record.Attempt) bool {
	return a.FullResponse && a.Status != nil && ok2xx(*a.Status) && a.ErrKind == ""
}

// ReconcileAttempts matches the run's order attempts with the access log.
func ReconcileAttempts(run string, atts []record.Attempt, lines []IngressLine) AttemptTable {
	t := AttemptTable{Diff: map[string]int{CatClientTimeout: 0, CatIngressRetry: 0, CatOther: 0, CatNotViaIngress: 0, CatUnexplained: 0}}
	prefix := run + "-a" // order attempts; the kitchen's are <run>-k-a...
	byID := map[string]IngressLine{}
	seen := map[string]int{}
	for _, l := range lines {
		a := l.attempt()
		if !l.isOrders() || !strings.HasPrefix(a, prefix) {
			t.Diff[CatOther]++
			continue
		}
		seen[a]++
		if seen[a] > 1 {
			t.DupLines++
			continue
		}
		byID[a] = l
	}
	t.Ingress = len(byID)
	unexplained := func(format string, a ...any) {
		t.Diff[CatUnexplained]++
		t.UnexplainedList = append(t.UnexplainedList, fmt.Sprintf(format, a...))
	}
	rhIDs := map[string]bool{}
	type refState struct{ first, last *record.Attempt }
	refs := map[string]*refState{}
	var refOrder []string
	for i := range atts {
		a := &atts[i]
		if a.Kind != record.KindOrder || a.Run != run {
			continue
		}
		t.RushHour++
		rhIDs[a.Attempt] = true
		st := refs[a.ClientRef]
		if st == nil {
			st = &refState{}
			refs[a.ClientRef] = st
			refOrder = append(refOrder, a.ClientRef)
		}
		if a.Try == 1 {
			st.first = a
		}
		if st.last == nil || a.Try > st.last.Try {
			st.last = a
		}
		l, in := byID[a.Attempt]
		switch {
		case !in && !a.FullResponse && a.ErrKind != "":
			// refused / reset / timeout with nothing logged at the ingress.
			t.Diff[CatNotViaIngress]++
		case !in:
			unexplained("%s：Rush Hour 收到 %s，入口沒有紀錄", a.Attempt, statusText(a))
		default:
			t.Matched++
			if l.upstreamAttempts() > 1 {
				t.Diff[CatIngressRetry]++
			}
			switch {
			case a.ErrKind == record.ErrTimeout:
				t.Diff[CatClientTimeout]++
			case a.FullResponse && a.Status != nil && *a.Status == l.status():
				t.SameStatus++
			default:
				unexplained("%s：Rush Hour %s，入口 %d %s（%s）", a.Attempt, statusText(a), l.status(), l.ResponseFlags, l.ResponseCodeDetails)
			}
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if !rhIDs[id] {
			l := byID[id]
			unexplained("%s：入口有紀錄（%d %s），Rush Hour 沒有這次嘗試", id, l.status(), l.ResponseFlags)
		}
	}
	lineOK := func(a *record.Attempt) bool {
		l, in := byID[a.Attempt]
		return in && ok2xx(l.status())
	}
	for _, ref := range refOrder {
		st := refs[ref]
		if st.first != nil {
			if !rhOK(*st.first) {
				t.FirstTryFailedRH++
			}
			if !lineOK(st.first) {
				t.FirstTryFailedIngress++
			}
		}
		if !rhOK(*st.last) {
			t.FailedAfterRetryRH++
		}
		if !lineOK(st.last) {
			t.FailedAfterRetryIngress++
		}
	}
	return t
}

func statusText(a *record.Attempt) string {
	switch {
	case a.ErrKind != "" && a.Status != nil:
		return fmt.Sprintf("%d 但回應不完整（%s）", *a.Status, a.ErrKind)
	case a.ErrKind != "":
		return "沒有回應（" + a.ErrKind + "）"
	case a.Status != nil:
		return strconv.Itoa(*a.Status)
	}
	return "沒有回應"
}

// ---------------------------------------------------------------- Envoy stats

// Stats is one envoy-stats-*.txt: koupa cluster counters summed over
// every agent ("# collected_at=<unix> node=..." starts each agent).
type Stats struct {
	Agents      int
	First, Last time.Time // earliest and latest collected_at
	v           map[string]float64
}

var (
	statLine  = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)\{(.*)\}\s+(\S+)$`)
	collected = regexp.MustCompile(`collected_at=([0-9]+(?:\.[0-9]+)?)`)
	labelRe   = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)
)

// statKey: the metric name plus its labels other than the cluster name,
// sorted, so agents and clusters of the Gateway add up.
func statKey(name, labels string) string {
	var ls []string
	for _, m := range labelRe.FindAllStringSubmatch(labels, -1) {
		if m[1] == "envoy_cluster_name" {
			continue
		}
		ls = append(ls, m[1]+`="`+m[2]+`"`)
	}
	sort.Strings(ls)
	return name + "{" + strings.Join(ls, ",") + "}"
}

// ParseStats reads a stats file. An error when no agent header is found.
func ParseStats(r io.Reader) (Stats, error) {
	s := Stats{v: map[string]float64{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "#") {
			m := collected.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			f, _ := strconv.ParseFloat(m[1], 64)
			sec := int64(f)
			at := time.Unix(sec, int64((f-float64(sec))*1e9)).UTC()
			if s.Agents == 0 || at.Before(s.First) {
				s.First = at
			}
			if s.Agents == 0 || at.After(s.Last) {
				s.Last = at
			}
			s.Agents++
			continue
		}
		m := statLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		f, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		s.v[statKey(m[1], m[2])] += f
	}
	if err := sc.Err(); err != nil {
		return s, err
	}
	if s.Agents == 0 {
		return s, errors.New("no agent section (# collected_at=...)")
	}
	return s, nil
}

// Get: the summed counter; labels as `k="v"` pairs (comma-separated,
// without the cluster name), "" for none. Absent counters read 0.
func (s Stats) Get(name, labels string) float64 {
	return s.v[statKey(name, labels)]
}

// StatsRow compares one Envoy counter delta with what the access log
// predicts for it.
type StatsRow struct {
	Key, Label  string
	Envoy, Log  int            // Envoy delta; access-log count for the same thing
	Retry       int            // ingress-internal retries that may explain Envoy > log
	Explained   map[string]int // difference explained, per category
	Unexplained int            // |Envoy - Log| minus what is explained
	Note        string
}

// StatsTable is the cluster-wide comparison.
type StatsTable struct {
	Window        [2]time.Time // stats start (earliest agent) .. end (latest agent)
	Agents        int
	InWindow      int // access-log lines that started and ended inside the window
	OutsideWindow int
	PathCounts    map[string]int // in-window lines per path group
	Rows          []StatsRow
}

func pathGroup(l IngressLine) string {
	p := l.Path
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	switch {
	case l.isOrders():
		return "POST /orders"
	case strings.HasPrefix(p, "/orders/") && strings.HasSuffix(p, "/ready"):
		return l.Method + " /orders/{id}/ready"
	case p == "/events":
		return l.Method + " /events"
	}
	return l.Method + " " + p
}

// CompareStats compares the counter deltas (end - start) with the
// access-log lines inside the statistics window.
func CompareStats(start, end Stats, lines []IngressLine) StatsTable {
	t := StatsTable{Window: [2]time.Time{start.First, end.Last}, Agents: end.Agents, PathCounts: map[string]int{}}
	d := func(name, labels string) int { return int(end.Get(name, labels) - start.Get(name, labels) + 0.5) }
	var (
		n, retries, uf, dc, local int
		xx                        = map[int]int{}
		ordersLines, ordersRetry  int
	)
	for _, l := range lines {
		st, ok := l.start()
		fin := st.Add(time.Duration(l.durationMs()) * time.Millisecond)
		if !ok || st.Before(start.First) || fin.After(end.Last) {
			t.OutsideWindow++
			continue
		}
		t.InWindow++
		n++
		t.PathCounts[pathGroup(l)]++
		ua := l.upstreamAttempts()
		if ua > 1 {
			retries += ua - 1
		}
		if l.isOrders() {
			ordersLines++
			if ua > 1 {
				ordersRetry += ua - 1
			}
		}
		if l.hasFlag("UF") {
			uf++
		}
		if l.status() == 0 {
			dc++
		} else {
			xx[l.status()/100]++
		}
		if ua == 0 && (l.UpstreamHost == "" || l.UpstreamHost == "-") {
			local++
		}
	}
	mk := func(key, label string, envoy, log, retry int, explained map[string]int, note string) StatsRow {
		if explained == nil {
			explained = map[string]int{}
		}
		diff := envoy - log
		if diff > 0 && retry > 0 {
			e := min(diff, retry)
			explained[CatIngressRetry] += e
			diff -= e
		}
		if diff < 0 {
			diff = -diff
		}
		return StatsRow{Key: key, Label: label, Envoy: envoy, Log: log, Retry: retry, Explained: explained, Unexplained: diff, Note: note}
	}
	// Every downstream request that reached the upstream stage completes
	// once per upstream attempt (connection failures included).
	upstreamLog := n - local
	t.Rows = append(t.Rows,
		mk("completed", "上游請求結束（upstream_rq_completed）", d("envoy_cluster_upstream_rq_completed", ""), upstreamLog, retries, nil,
			"access log：視窗內的行數（不含入口自己產生、沒有上游的回應），入口內部重試每次另算一筆"),
		mk("rq_total", "送到上游的請求（upstream_rq_total）", d("envoy_cluster_upstream_rq_total", ""), upstreamLog-uf, retries, nil,
			"access log：同上，扣掉連不上上游（UF）的行；那些 Envoy 記在 upstream_rq_pending_failure_eject"),
		mk("uf", "連不上上游（upstream_rq_pending_failure_eject）", d("envoy_cluster_upstream_rq_pending_failure_eject", ""), uf, 0, nil,
			"access log：response_flags 含 UF 的行"),
		mk("cancelled", "客人端先斷線（upstream_rq_cancelled）", d("envoy_cluster_upstream_rq_cancelled", ""), dc, 0, nil,
			"access log：status 0（DC，客人端逾時或斷線）；"+CatClientTimeout),
	)
	for _, c := range []int{2, 4, 5} {
		label := fmt.Sprintf("上游回應 %dxx（upstream_rq_xx）", c)
		t.Rows = append(t.Rows, mk(fmt.Sprintf("%dxx", c), label, d("envoy_cluster_upstream_rq_xx", fmt.Sprintf(`envoy_response_code_class="%d"`, c)), xx[c], retries, nil,
			"access log：最終狀態碼；入口內部重試的中間回應只在 Envoy 計數，可能落在任一類；"+
				"/events 長連線在回應開始時就計入 Envoy，access log 要等串流結束才寫，視窗邊界上的串流可能只出現在一邊"))
	}
	// /orders share: the counter has no path, so take the other paths off.
	other := upstreamLog - ordersLines
	t.Rows = append(t.Rows, mk("orders_share", "其中 POST /orders（Envoy 扣掉其他路徑）",
		d("envoy_cluster_upstream_rq_completed", "")-(other+(retries-ordersRetry)), ordersLines, ordersRetry,
		map[string]int{CatOther: other},
		fmt.Sprintf("Envoy 是整個 cluster 的計數；先扣掉 access log 裡其他路徑的 %d 行（%s）及其內部重試", other, CatOther)))
	return t
}

// ---------------------------------------------------------------- output

// Result is what --json writes.
type Result struct {
	Dir            string       `json:"dir"`
	Run            string       `json:"run"`
	IngressBad     int          `json:"ingress_bad_lines"`
	RereadLines    int          `json:"reread_duplicate_lines"`
	Attempts       AttemptTable `json:"attempts"`
	Stats          *StatsTable  `json:"stats,omitempty"`
	StatsError     string       `json:"stats_error,omitempty"`
	TimeoutSeconds float64      `json:"rush_hour_timeout_s"`
}

func render(w io.Writer, r Result) {
	a := r.Attempts
	fmt.Fprintf(w, "入口對帳（A14）　量測編號 %s\n證據：%s\n\n", r.Run, r.Dir)
	if r.RereadLines > 0 {
		fmt.Fprintf(w, "入口紀錄裡完全相同的重複行 %d 行已去掉（收集端重新讀取串流，見 missing.jsonl）\n\n", r.RereadLines)
	}
	fmt.Fprintln(w, "【一】逐筆：POST /orders，同一次量測（X-Koupa-Attempt 屬於這次），最終回應")
	fmt.Fprintf(w, "  Rush Hour 下單嘗試           %6d\n", a.RushHour)
	fmt.Fprintf(w, "  入口紀錄（access log）        %6d\n", a.Ingress)
	fmt.Fprintf(w, "  兩邊都有                      %6d（狀態碼一致 %d）\n", a.Matched, a.SameStatus)
	if a.DupLines > 0 {
		fmt.Fprintf(w, "  入口紀錄重複的行（收集端重讀）%6d（已去掉）\n", a.DupLines)
	}
	fmt.Fprintln(w, "  差額分類：")
	for _, c := range []string{CatClientTimeout, CatIngressRetry, CatOther, CatNotViaIngress, CatUnexplained} {
		note := map[string]string{
			CatClientTimeout: "Rush Hour 逾時放棄，入口仍有紀錄（DC 或之後才回應）",
			CatIngressRetry:  "兩邊都有，但入口送了上游不只一次（upstream_attempts > 1）；不影響逐筆對應",
			CatOther:         "不在範圍內的入口紀錄：其他路徑、其他量測、沒有嘗試編號的請求",
			CatNotViaIngress: "Rush Hour 沒有回應（refused/reset/timeout），入口沒有任何紀錄",
			CatUnexplained:   "以上都解釋不了，逐筆列在下面",
		}[c]
		fmt.Fprintf(w, "    %-14s %6d　%s\n", c, a.Diff[c], note)
	}
	for _, s := range a.UnexplainedList {
		fmt.Fprintf(w, "      - %s\n", s)
	}
	fmt.Fprintln(w, "  送出沒有成功回應（client_ref 數）　Rush Hour／依入口紀錄")
	fmt.Fprintf(w, "    第一次送出沒有成功回應       %6d／%d\n", a.FirstTryFailedRH, a.FirstTryFailedIngress)
	fmt.Fprintf(w, "    重試後仍沒有成功回應         %6d／%d\n", a.FailedAfterRetryRH, a.FailedAfterRetryIngress)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "【二】Envoy 統計（cluster 層級，沒有路徑維度）對 access log 的全部流量")
	if r.Stats == nil {
		fmt.Fprintf(w, "  Envoy 統計取不到：%s\n", r.StatsError)
		return
	}
	s := r.Stats
	fmt.Fprintf(w, "  統計視窗 %s ～ %s（%d 個 agent）；視窗內 access log %d 行，視窗外 %d 行不算\n",
		s.Window[0].Format("15:04:05.000"), s.Window[1].Format("15:04:05.000"), s.Agents, s.InWindow, s.OutsideWindow)
	var groups []string
	for g := range s.PathCounts {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		fmt.Fprintf(w, "    %-28s %6d 行\n", g, s.PathCounts[g])
	}
	fmt.Fprintf(w, "  %-44s %8s %8s %8s  %s\n", "項目", "Envoy", "access", "對不上", "已解釋的差額")
	for _, row := range s.Rows {
		var ex []string
		for _, c := range []string{CatClientTimeout, CatIngressRetry, CatOther, CatNotViaIngress} {
			if v := row.Explained[c]; v > 0 {
				ex = append(ex, fmt.Sprintf("%s %d", c, v))
			}
		}
		fmt.Fprintf(w, "  %-44s %8d %8d %8d  %s\n", row.Label, row.Envoy, row.Log, row.Unexplained, strings.Join(ex, "、"))
		fmt.Fprintf(w, "      %s\n", row.Note)
	}
	fmt.Fprintln(w, "  （"+CatNotViaIngress+"的嘗試兩邊都沒有，見【一】。）")
}

func runMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ingress-reconcile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.String("json", "", "also write the tables as JSON to FILE")
	runID := fs.String("run", "", "run id (default: params.json run_id)")
	timeout := fs.Duration("timeout", 5*time.Second, "Rush Hour's per-attempt timeout (for the record)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: ingress-reconcile [--json FILE] [--run ID] EVIDENCE_DIR")
		return 2
	}
	dir := fs.Arg(0)
	f, err := os.Open(filepath.Join(dir, "ingress.log"))
	if err != nil {
		fmt.Fprintf(stdout, "入口紀錄取不到（%v）\n", err)
		return 3
	}
	lines, bad := ParseIngress(f)
	f.Close()
	lines, reread := DedupeLines(lines)
	if len(lines) == 0 {
		fmt.Fprintln(stdout, "入口紀錄取不到（ingress.log 沒有任何紀錄）")
		return 3
	}
	run := *runID
	if run == "" {
		var p struct {
			RunID string `json:"run_id"`
		}
		if b, err := os.ReadFile(filepath.Join(dir, "params.json")); err == nil {
			_ = json.Unmarshal(b, &p)
		}
		run = p.RunID
	}
	if run == "" {
		fmt.Fprintln(stderr, "ingress-reconcile: no run id (params.json run_id or --run)")
		return 2
	}
	atts, err := readAttempts(filepath.Join(dir, "attempts.jsonl"))
	if err != nil {
		fmt.Fprintln(stderr, "ingress-reconcile:", err)
		return 2
	}
	res := Result{Dir: dir, Run: run, IngressBad: bad, RereadLines: reread, Attempts: ReconcileAttempts(run, atts, lines), TimeoutSeconds: timeout.Seconds()}
	s0, err0 := readStats(filepath.Join(dir, "envoy-stats-start.txt"))
	s1, err1 := readStats(filepath.Join(dir, "envoy-stats-end.txt"))
	if err := errors.Join(err0, err1); err != nil {
		res.StatsError = err.Error()
	} else {
		st := CompareStats(s0, s1, lines)
		res.Stats = &st
	}
	render(stdout, res)
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(res, "", "  ")
		if err := os.WriteFile(*jsonOut, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintln(stderr, "ingress-reconcile:", err)
			return 2
		}
	}
	return 0
}

func readAttempts(path string) ([]record.Attempt, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []record.Attempt
	for _, l := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		var a record.Attempt
		if json.Unmarshal(l, &a) == nil {
			out = append(out, a)
		}
	}
	return out, nil
}

func readStats(path string) (Stats, error) {
	f, err := os.Open(path)
	if err != nil {
		return Stats{}, err
	}
	defer f.Close()
	s, err := ParseStats(f)
	if err != nil {
		return s, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return s, nil
}
