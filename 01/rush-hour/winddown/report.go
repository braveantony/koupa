package winddown

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"koupa/rush-hour/record"
	"koupa/rush-hour/verdict"
)

// Facts are what the wind-down observed that is not in the JSONL files.
// They are stored in scorecard.json so a scorecard can be recomputed.
type Facts struct {
	Run         string    `json:"run"`
	TxTimeoutMs int64     `json:"tx_timeout_ms"`
	EvaluatedAt time.Time `json:"evaluated_at"`
	// NoIngress: no ingress in the path (direct to the App, local/test
	// only); every attempt counts as exactly one upstream attempt.
	NoIngress bool  `json:"no_ingress"`
	Retries   int   `json:"retries"`
	Audit     Audit `json:"audit"`
	// CaughtUp: board name -> its max seq reached the cursor in time.
	CaughtUp map[string]bool `json:"caught_up"`
	// ClientInFlight: client_refs still being sent at the client limit;
	// their attempts that ended at or after ClientCancelAt without a full
	// response were cut by the wind-down, not failed.
	ClientInFlight  []string   `json:"client_in_flight,omitempty"`
	ClientCancelAt  *time.Time `json:"client_cancel_at,omitempty"`
	BoardsStoppedAt *time.Time `json:"boards_stopped_at,omitempty"`
	// Invalid lists reasons found while running (evidence writes failed,
	// a board never got hello, ...).
	Invalid     []string        `json:"invalid,omitempty"`
	Notes       []string        `json:"notes,omitempty"`
	Timeline    []TimelineEntry `json:"timeline,omitempty"`
	Host        *HostStats      `json:"host,omitempty"`
	IngressPath string          `json:"ingress_log,omitempty"`
	AppLogsDir  string          `json:"app_logs,omitempty"`
}

// Audit is the result of /api/audit (A9 step 5). nil fields mean the
// query failed.
type Audit struct {
	Cursor *int64             `json:"cursor"`
	Orders map[string][]int64 `json:"orders"`
	Ready  []AuditReady       `json:"ready"`
	Errors []string           `json:"errors,omitempty"`
}

// AuditReady is one row of mode=ready.
type AuditReady struct {
	OrderID      int64   `json:"order_id"`
	ReadySeq     int64   `json:"ready_seq"`
	ReadyAttempt *string `json:"ready_attempt"`
}

// TimelineEntry is one point of the run's timeline.
type TimelineEntry struct {
	At   time.Time `json:"at"`
	What string    `json:"what"`
}

// HostStats summarises host resources sampled during the run.
type HostStats struct {
	Samples        int     `json:"samples"`
	MaxCPUPct      float64 `json:"max_cpu_pct"`
	MinMemAvailPct float64 `json:"min_mem_avail_pct"`
	Err            string  `json:"err,omitempty"`
}

// ReadFacts reads a facts JSON file (testdata, or scorecard.json's facts).
func ReadFacts(path string) (Facts, error) {
	var f Facts
	b, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(b, &f)
}

// Item is one score: confirmed plus unknown.
type Item struct {
	Confirmed int `json:"confirmed"`
	Unknown   int `json:"unknown"`
}

func (i Item) count() verdict.Count { return verdict.Count{Confirmed: i.Confirmed, Unknown: i.Unknown} }
func item(c verdict.Count) Item     { return Item{c.Confirmed, c.Unknown} }

// Items are the main scores (design 5.3 主要成績).
type Items struct {
	Orders           Item            `json:"orders"`
	FirstTryFailed   Item            `json:"first_try_failed"`
	FailedAfterRetry Item            `json:"failed_after_retry"`
	DuplicateRefs    Item            `json:"duplicate_client_refs"`
	DuplicateExtra   Item            `json:"duplicate_extra_orders"`
	Missing          map[string]Item `json:"missing"`
}

func (it Items) anyUnknown() bool {
	for _, i := range []Item{it.Orders, it.FirstTryFailed, it.FailedAfterRetry, it.DuplicateRefs, it.DuplicateExtra} {
		if i.Unknown > 0 {
			return true
		}
	}
	for _, i := range it.Missing {
		if i.Unknown > 0 {
			return true
		}
	}
	return false
}

// FailedAttempt is a failed attempt with its A10 state and cause.
type FailedAttempt struct {
	Attempt   string            `json:"attempt"`
	Kind      string            `json:"kind"`
	ClientRef string            `json:"client_ref"`
	Status    *int              `json:"status,omitempty"`
	ErrKind   string            `json:"err_kind,omitempty"`
	PodUID    string            `json:"pod_uid,omitempty"`
	Cut       bool              `json:"cut_by_winddown,omitempty"`
	State     verdict.StateKind `json:"state"`
	Cause     verdict.CauseKind `json:"cause"`
}

// UnsettledAttempt is an attempt whose server-side result is unknown.
type UnsettledAttempt struct {
	Attempt   string `json:"attempt"`
	Kind      string `json:"kind"`
	ClientRef string `json:"client_ref"`
	Reason    string `json:"reason"`
}

// BoardDiag is per-board diagnostics.
type BoardDiag struct {
	Start           *int64  `json:"start"`
	Received        int     `json:"received"`
	CaughtUp        bool    `json:"caught_up"`
	Complete        bool    `json:"complete"`
	AfterShutdown   int     `json:"disconnect_after_shutdown"`
	WithoutShutdown int     `json:"disconnect_without_shutdown"`
	NotIncreasing   bool    `json:"not_increasing"`
	Duplicates      []int64 `json:"duplicates,omitempty"`
}

// PodDiag is per-Pod diagnostics.
type PodDiag struct {
	UID         string     `json:"uid"`
	Name        string     `json:"name,omitempty"`
	Covered     bool       `json:"log_covered"`
	Coverage    string     `json:"coverage"`
	SigtermAt   *time.Time `json:"sigterm_at,omitempty"`
	ExitAt      *time.Time `json:"exit_at,omitempty"`
	PostSigterm *int       `json:"post_sigterm_requests"` // nil: no sigterm line
	TestFlags   []string   `json:"test_flags,omitempty"`
}

// Diag is the 診斷資訊 section.
type Diag struct {
	Failed         []FailedAttempt      `json:"failed_attempts"`
	Unsettled      []UnsettledAttempt   `json:"unsettled_attempts"`
	ClientInFlight []string             `json:"client_in_flight"`
	NotCaughtUp    []string             `json:"boards_not_caught_up"`
	PendingOrders  *int                 `json:"pending_orders"` // nil: audit failed
	Boards         map[string]BoardDiag `json:"boards"`
	Pods           []PodDiag            `json:"pods"`
	Ingress        string               `json:"ingress"`
	AppLogs        string               `json:"app_logs"`
	Timeline       []TimelineEntry      `json:"timeline"`
	Host           *HostStats           `json:"host,omitempty"`
	SchedLagMaxMs  int64                `json:"sched_lag_max_ms"`
	SchedLagP99Ms  int64                `json:"sched_lag_p99_ms"`
	ResourceTight  bool                 `json:"resource_tight"`
	Notes          []string             `json:"notes,omitempty"`
}

// Scorecard is one run's result; it is scorecard.json.
type Scorecard struct {
	Run            string   `json:"run"`
	Invalid        bool     `json:"invalid"`
	InvalidReason  string   `json:"invalid_reason"`
	InvalidReasons []string `json:"invalid_reasons"`
	// HasUnknown: some score or server-side result could not be decided
	// (A13: a control run with HasUnknown does not pass).
	HasUnknown bool  `json:"has_unknown"`
	NoIngress  bool  `json:"no_ingress"`
	Items      Items `json:"items"`
	Diag       Diag  `json:"diagnostics"`
	Facts      Facts `json:"facts"`
}

// ExitCode is 2 for an invalid measurement, else 0.
func (sc Scorecard) ExitCode() int {
	if sc.Invalid {
		return 2
	}
	return 0
}

// Resource thresholds (provisional until measured, A10).
const (
	cpuTightPct     = 90.0
	memTightPct     = 10.0
	schedLagTightMs = 100
)

// Evaluate reconciles the evidence (A9 step 7) at f.EvaluatedAt.
func Evaluate(f Facts, ev Evidence) Scorecard {
	sc := Scorecard{Run: f.Run, NoIngress: f.NoIngress, Facts: f}
	invalid := append([]string(nil), f.Invalid...)
	if ev.AttemptsErr != nil {
		invalid = append(invalid, "attempts.jsonl 讀不到："+ev.AttemptsErr.Error())
	}
	if ev.AttemptsBad > 0 {
		invalid = append(invalid, fmt.Sprintf("attempts.jsonl 有 %d 行無法解析", ev.AttemptsBad))
	}
	if ev.BoardsErr != nil {
		invalid = append(invalid, "boards.jsonl 讀不到："+ev.BoardsErr.Error())
	}
	auditOK := f.Audit.Cursor != nil && f.Audit.Orders != nil && f.Audit.Ready != nil
	if !auditOK {
		msg := "audit 查詢失敗"
		if len(f.Audit.Errors) > 0 {
			msg += "：" + strings.Join(f.Audit.Errors, "；")
		}
		invalid = append(invalid, msg)
	}
	txTimeout := time.Duration(f.TxTimeoutMs) * time.Millisecond
	now := f.EvaluatedAt

	inFlight := map[string]bool{}
	for _, r := range f.ClientInFlight {
		inFlight[r] = true
	}
	cut := func(a record.Attempt) bool {
		return inFlight[a.ClientRef] && !a.FullResponse && f.ClientCancelAt != nil && !a.RespondedAt.Before(*f.ClientCancelAt)
	}
	dbReady := map[int64]*string{}
	seqOf := map[int64]int64{}
	if auditOK {
		for _, r := range f.Audit.Ready {
			dbReady[r.OrderID] = r.ReadyAttempt
			seqOf[r.OrderID] = r.ReadySeq
		}
	} else {
		dbReady = nil
	}

	// Settlement and A10 state of every order and ready attempt.
	unsettledRefs := map[string]bool{}  // order attempts not settled
	unsettledReady := map[string]bool{} // refs with a ready attempt not settled
	var lags []int64
	type refAgg struct {
		tries []record.Attempt
	}
	refs := map[string]*refAgg{}
	var refOrder []string
	for _, a := range ev.Attempts {
		if a.Kind != record.KindOrder && a.Kind != record.KindReady {
			continue
		}
		lags = append(lags, a.SchedLagMs)
		e := assemble(a, ev, f.NoIngress, dbReady)
		s := verdict.Settle(e, now, txTimeout)
		if !s.Settled {
			sc.Diag.Unsettled = append(sc.Diag.Unsettled, UnsettledAttempt{a.Attempt, a.Kind, a.ClientRef, s.Reason})
			if a.Kind == record.KindOrder {
				unsettledRefs[a.ClientRef] = true
			} else {
				unsettledReady[a.ClientRef] = true
			}
		}
		if !e.FullSuccess {
			gotErr := a.FullResponse && a.Status != nil && *a.Status >= 400
			st := verdict.State(e, gotErr)
			sc.Diag.Failed = append(sc.Diag.Failed, FailedAttempt{
				Attempt: a.Attempt, Kind: a.Kind, ClientRef: a.ClientRef, Status: a.Status,
				ErrKind: a.ErrKind, PodUID: a.PodUID, Cut: cut(a), State: st, Cause: verdict.Cause(e, st),
			})
		}
		if a.Kind == record.KindOrder {
			ag := refs[a.ClientRef]
			if ag == nil {
				ag = &refAgg{}
				refs[a.ClientRef] = ag
				refOrder = append(refOrder, a.ClientRef)
			}
			ag.tries = append(ag.tries, a)
		}
	}

	// Client-side scores: what the customer saw.
	sc.Items.Orders = Item{Confirmed: len(refs)}
	ok := func(a record.Attempt) bool { return a.FullResponse && a.Status != nil && *a.Status/100 == 2 }
	for _, ref := range refOrder {
		tries := refs[ref].tries
		sort.SliceStable(tries, func(i, j int) bool { return tries[i].Try < tries[j].Try })
		first := tries[0]
		if first.Try != 1 || cut(first) {
			// No record of the first try, or it was cut at the client limit.
			sc.Items.FirstTryFailed.Unknown++
			sc.Items.FailedAfterRetry.Unknown++
			continue
		}
		if ok(first) {
			continue
		}
		sc.Items.FirstTryFailed.Confirmed++
		last := tries[len(tries)-1]
		switch {
		case ok(last):
		case cut(last) || (inFlight[ref] && last.Try < 1+f.Retries):
			sc.Items.FailedAfterRetry.Unknown++
		default:
			sc.Items.FailedAfterRetry.Confirmed++
		}
	}
	for _, ref := range f.ClientInFlight {
		if refs[ref] == nil {
			sc.Items.Orders.Unknown++ // scheduled, never recorded
		}
	}
	sc.Diag.ClientInFlight = append([]string{}, f.ClientInFlight...)

	// Duplicates and missing (database side).
	if auditOK {
		cr, ex := verdict.Duplicates(f.Audit.Orders, unsettledRefs)
		sc.Items.DuplicateRefs, sc.Items.DuplicateExtra = item(cr), item(ex)
		pending := 0
		for _, ids := range f.Audit.Orders {
			for _, id := range ids {
				if _, has := seqOf[id]; !has {
					pending++
				}
			}
		}
		sc.Diag.PendingOrders = &pending
	}
	unsettledSeqs := map[int64]bool{}
	if auditOK {
		for ref := range unsettledReady {
			for _, id := range f.Audit.Orders[ref] {
				if s, has := seqOf[id]; has {
					unsettledSeqs[s] = true
				}
			}
		}
	}
	boards := boardSeqs(ev, f)
	in := verdict.MissingInput{DBComplete: auditOK, Boards: boards, UnsettledSeqs: unsettledSeqs}
	if auditOK {
		in.Cursor = *f.Audit.Cursor
		for _, r := range f.Audit.Ready {
			in.DBReady = append(in.DBReady, verdict.ReadyRow{Seq: r.ReadySeq, OrderID: r.OrderID})
		}
	}
	mr := verdict.Missing(in)
	sc.Items.Missing = map[string]Item{}
	sc.Diag.Boards = map[string]BoardDiag{}
	for _, b := range boards {
		if b.Start == nil {
			invalid = append(invalid, "看板 "+b.Name+" 沒有起點（沒收到 hello）")
		} else {
			sc.Items.Missing[b.Name] = item(mr.PerBoard[b.Name])
		}
		if !b.CaughtUp {
			sc.Diag.NotCaughtUp = append(sc.Diag.NotCaughtUp, b.Name)
		}
		d := sc.Diag.Boards[b.Name]
		d.Start, d.Received, d.CaughtUp, d.Complete = b.Start, len(b.Received), b.CaughtUp, b.Complete
		d.NotIncreasing, d.Duplicates = mr.NotIncreasing[b.Name], mr.Duplicates[b.Name]
		sc.Diag.Boards[b.Name] = d
	}
	if mr.Invalid && auditOK && len(invalid) == 0 {
		invalid = append(invalid, "看板對帳資料不足")
	}
	disconnects(ev, f, sc.Diag.Boards)

	// Pods, ingress, App logs.
	if ev.Ingress != nil && ev.Ingress.Available {
		sc.Diag.Ingress = fmt.Sprintf("入口紀錄 %d 筆嘗試", len(ev.Ingress.Recs))
		if ev.Ingress.Bad > 0 {
			sc.Diag.Ingress += fmt.Sprintf("，%d 行無法解析", ev.Ingress.Bad)
		}
		if len(ev.Ingress.Dups) > 0 {
			sc.Diag.Ingress += fmt.Sprintf("，%d 個嘗試編號重複出現（視為未知）", len(ev.Ingress.Dups))
		}
	} else {
		reason := ""
		if ev.Ingress != nil {
			reason = ev.Ingress.Err
		}
		sc.Diag.Ingress = "入口紀錄取不到（" + reason + "）"
	}
	if f.NoIngress {
		sc.Diag.Ingress = "--no-ingress：直接連 App，沒有入口紀錄；每次嘗試的上游嘗試次數一律當成 1（只供本機測試）"
	}
	if ev.App != nil && ev.App.Available {
		sc.Diag.AppLogs = fmt.Sprintf("App log %d 個 Pod", len(ev.App.Pods))
		var uids []string
		for uid := range ev.App.Pods {
			uids = append(uids, uid)
		}
		sort.Strings(uids)
		for _, uid := range uids {
			p := ev.App.Pods[uid]
			pd := PodDiag{UID: uid, Name: p.Name, Covered: p.Covered, Coverage: p.describe(),
				SigtermAt: p.SigtermAt, ExitAt: p.ExitAt, TestFlags: p.TestFlags}
			if p.SigtermAt != nil {
				n := p.PostSigterm
				pd.PostSigterm = &n
			}
			sc.Diag.Pods = append(sc.Diag.Pods, pd)
			if len(p.TestFlags) > 0 {
				sc.Diag.Notes = append(sc.Diag.Notes, "Pod "+uid+" 開著 "+strings.Join(p.TestFlags, "、")+"：正式量測不可以開")
			}
		}
	} else {
		reason := ""
		if ev.App != nil {
			reason = ev.App.Err
		}
		sc.Diag.AppLogs = "App log 取不到（" + reason + "）"
	}

	// Timeline, host, scheduling lag.
	sc.Diag.Timeline = timeline(f, ev)
	sc.Diag.Host = f.Host
	sc.Diag.SchedLagMaxMs, sc.Diag.SchedLagP99Ms = lagStats(lags)
	if h := f.Host; h != nil && h.Samples > 0 && (h.MaxCPUPct >= cpuTightPct || h.MinMemAvailPct <= memTightPct) {
		sc.Diag.ResourceTight = true
	}
	if sc.Diag.SchedLagP99Ms >= schedLagTightMs {
		sc.Diag.ResourceTight = true
	}
	sc.Diag.Notes = append(sc.Diag.Notes, f.Notes...)
	if sc.Diag.Failed == nil {
		sc.Diag.Failed = []FailedAttempt{}
	}
	if sc.Diag.Unsettled == nil {
		sc.Diag.Unsettled = []UnsettledAttempt{}
	}

	sc.InvalidReasons = invalid
	if sc.InvalidReasons == nil {
		sc.InvalidReasons = []string{}
	}
	sc.Invalid = len(invalid) > 0
	sc.InvalidReason = strings.Join(invalid, "；")
	sc.HasUnknown = sc.Items.anyUnknown() || len(sc.Diag.Unsettled) > 0 ||
		len(sc.Diag.ClientInFlight) > 0 || len(sc.Diag.NotCaughtUp) > 0
	return sc
}

// boardSeqs rebuilds each board's view from boards.jsonl.
func boardSeqs(ev Evidence, f Facts) []verdict.BoardSeq {
	byName := map[string]*verdict.BoardSeq{}
	var names []string
	complete := ev.BoardsErr == nil && ev.BoardsBad == 0
	for _, e := range ev.Boards {
		b := byName[e.Board]
		if b == nil {
			b = &verdict.BoardSeq{Name: e.Board, Complete: complete, CaughtUp: f.CaughtUp[e.Board]}
			byName[e.Board] = b
			names = append(names, e.Board)
		}
		switch e.Kind {
		case record.BoardHello:
			if b.Start == nil && e.Start != nil {
				v := *e.Start
				b.Start = &v
			}
		case record.BoardReady:
			if e.ReadySeq != nil {
				b.Received = append(b.Received, *e.ReadySeq)
			}
		}
	}
	// Boards that the wind-down knows of but that left no record at all.
	for name := range f.CaughtUp {
		if byName[name] == nil {
			byName[name] = &verdict.BoardSeq{Name: name, Complete: complete}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]verdict.BoardSeq, 0, len(names))
	for _, n := range names {
		out = append(out, *byName[n])
	}
	return out
}

// disconnects counts disconnect kinds per board, leaving out the ones caused
// by stopping the boards at the end of the run.
func disconnects(ev Evidence, f Facts, boards map[string]BoardDiag) {
	for _, e := range ev.Boards {
		if e.Kind != record.BoardDisconnect {
			continue
		}
		if f.BoardsStoppedAt != nil && !e.At.Before(*f.BoardsStoppedAt) {
			continue
		}
		d := boards[e.Board]
		if e.ShutdownSeen != nil && *e.ShutdownSeen {
			d.AfterShutdown++
		} else {
			d.WithoutShutdown++
		}
		boards[e.Board] = d
	}
}

func timeline(f Facts, ev Evidence) []TimelineEntry {
	out := append([]TimelineEntry(nil), f.Timeline...)
	if ev.App != nil {
		for uid, p := range ev.App.Pods {
			if p.SigtermAt != nil {
				out = append(out, TimelineEntry{*p.SigtermAt, "Pod " + uid + " 記下 SIGTERM（App log）"})
			}
			if p.ExitAt != nil {
				out = append(out, TimelineEntry{*p.ExitAt, "Pod " + uid + " 記下程式結束（App log）"})
			}
			if p.Meta != nil {
				if t := parseTime(p.Meta.TermStart); t != nil {
					out = append(out, TimelineEntry{*t, "Pod " + uid + " 推估終止起點"})
				}
				if t := parseTime(p.Meta.ExitedAt); t != nil {
					out = append(out, TimelineEntry{*t, "Pod " + uid + " container 結束（finishedAt）"})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func lagStats(lags []int64) (max, p99 int64) {
	if len(lags) == 0 {
		return 0, 0
	}
	s := append([]int64(nil), lags...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := (len(s)*99+99)/100 - 1
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[len(s)-1], s[idx]
}

// ---------------------------------------------------------------- text

var stateText = map[verdict.StateKind]string{
	verdict.ClientGotError:        "客人收到錯誤回應",
	verdict.NotReachedApp:         "沒送到 App",
	verdict.ReachedNotEnded:       "送到了，App 沒有結束這個請求",
	verdict.WriteFailedOrCanceled: "App 結束請求時寫入失敗或被取消",
	verdict.WriteFlushOK:          "客人沒有收到完整回應，但 App 回報寫入和 flush 成功",
	verdict.Unknown:               "未知",
}

var causeText = map[verdict.CauseKind]string{
	verdict.CauseUnknown:                "未知",
	verdict.CauseRoutedAfterTermination: "入口在 Pod 開始終止後仍選了它",
	verdict.CauseExitedMidRequest:       "請求處理到一半，程式就結束了",
}

func withUnit(i Item, unit string) string {
	if i.Unknown > 0 {
		return i.count().StringUnit(unit)
	}
	return fmt.Sprintf("%d %s", i.Confirmed, unit)
}

// dupText follows A9: 「已確認 N；另有 M 筆伺服器端結果未知，可能增加」.
func dupText(i Item, unit string) string {
	if i.Unknown > 0 {
		return fmt.Sprintf("已確認 %d；另有 %d 筆伺服器端結果未知，可能增加", i.Confirmed, i.Unknown)
	}
	if unit == "" {
		return fmt.Sprintf("%d", i.Confirmed)
	}
	return fmt.Sprintf("%d %s", i.Confirmed, unit)
}

func ts(t time.Time) string { return t.UTC().Format("15:04:05.000") }

// Text renders scorecard.txt: the main section, plus diagnostics when
// verbose. An invalid run starts with 「本次量測無效」 and has no scores.
func (sc Scorecard) Text(verbose bool) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	if sc.Invalid {
		w("本次量測無效：%s", sc.InvalidReason)
	}
	w("Rush Hour 成績單　量測編號 %s", sc.Run)
	if sc.NoIngress {
		w("注意：使用了 --no-ingress（直接連 App，只供本機測試）：每次嘗試的上游嘗試次數一律當成 1，不可用於正式量測")
	}
	if sc.Invalid {
		w("")
		w("這次量測的關鍵資料不足，不列出主要成績，也不可用於比較。")
	} else {
		it := sc.Items
		w("")
		w("【主要成績】")
		w("訂單：%s", withUnit(it.Orders, "張"))
		w("第一次送出沒有成功回應：%s", withUnit(it.FirstTryFailed, "張"))
		w("重試後仍沒有成功回應：%s", withUnit(it.FailedAfterRetry, "張"))
		w("重複訂單（client_ref 數）：%s", dupText(it.DuplicateRefs, ""))
		w("多出的訂單張數：%s", dupText(it.DuplicateExtra, "張"))
		w("大螢幕漏號（每台看板）：")
		for _, name := range sortedKeys(it.Missing) {
			w("  %s：%s", name, it.Missing[name].count().StringUnit("個叫號"))
		}
		if sc.HasUnknown {
			w("")
			w("有無法判定的項目（加上 --verbose 看原因）；有未知時不可以當成 0。")
		}
	}
	if verbose {
		sc.writeDiag(&b)
	}
	return b.String()
}

func (sc Scorecard) writeDiag(b *strings.Builder) {
	w := func(format string, a ...any) { fmt.Fprintf(b, format+"\n", a...) }
	d := sc.Diag
	w("")
	w("【診斷資訊】")
	if len(sc.InvalidReasons) > 0 {
		w("無效原因：")
		for _, r := range sc.InvalidReasons {
			w("  - %s", r)
		}
	}
	w("入口紀錄：%s", d.Ingress)
	w("App log：%s", d.AppLogs)
	for _, n := range d.Notes {
		w("注意：%s", n)
	}

	w("")
	if len(d.Failed) == 0 {
		w("失敗的嘗試：無")
	} else {
		w("失敗的嘗試（%d 次）：", len(d.Failed))
		for _, f := range d.Failed {
			st := "無回應"
			if f.Status != nil {
				st = fmt.Sprintf("HTTP %d", *f.Status)
			}
			if f.ErrKind != "" {
				st += "／" + f.ErrKind
			}
			cut := ""
			if f.Cut {
				cut = "（收尾時被中止）"
			}
			w("  %s %s %s：%s%s；狀態：%s；原因：%s", f.Attempt, f.Kind, f.ClientRef, st, cut, stateText[f.State], causeText[f.Cause])
		}
	}

	w("")
	w("大螢幕：")
	for _, name := range sortedKeys(d.Boards) {
		bd := d.Boards[name]
		start := "沒有起點"
		if bd.Start != nil {
			start = fmt.Sprintf("%d", *bd.Start)
		}
		inc := "否"
		switch {
		case bd.NotIncreasing:
			inc = "是"
		case !bd.Complete:
			inc = "未發現違反（紀錄不完整）"
		}
		caught := "是"
		if !bd.CaughtUp {
			caught = "否（沒追上）"
		}
		w("  %s：起點 %s，收到 %d 則，追上截止游標：%s", name, start, bd.Received, caught)
		w("    收到 shutdown 後斷線：%d；沒收到 shutdown 就斷線：%d", bd.AfterShutdown, bd.WithoutShutdown)
		dups := "無"
		if len(bd.Duplicates) > 0 {
			dups = fmt.Sprint(bd.Duplicates)
		}
		w("    編號不是嚴格遞增：%s；重複收到：%s", inc, dups)
	}

	w("")
	n := len(d.Unsettled) + len(d.ClientInFlight) + len(d.NotCaughtUp)
	if n == 0 {
		w("未確定的項目：無")
	} else {
		w("未確定的項目：")
		if len(d.ClientInFlight) > 0 {
			w("  用戶端在途（%d 張）：%s", len(d.ClientInFlight), strings.Join(d.ClientInFlight, ", "))
		}
		if len(d.NotCaughtUp) > 0 {
			w("  沒追上的看板：%s", strings.Join(d.NotCaughtUp, ", "))
		}
		if len(d.Unsettled) > 0 {
			w("  伺服器端結果未知（%d 次嘗試）：", len(d.Unsettled))
			for _, u := range d.Unsettled {
				w("    %s %s %s：%s", u.Attempt, u.Kind, u.ClientRef, u.Reason)
			}
		}
	}
	if d.PendingOrders != nil {
		w("還沒完成（沒有叫號）的訂單：%d 張", *d.PendingOrders)
	} else {
		w("還沒完成的訂單：查詢失敗")
	}

	w("")
	w("SIGTERM 之後才到的請求（每個 Pod）：")
	if len(d.Pods) == 0 {
		w("  沒有 App log")
	}
	for _, p := range d.Pods {
		cnt := "沒有 SIGTERM 紀錄"
		if p.PostSigterm != nil {
			cnt = fmt.Sprintf("%d", *p.PostSigterm)
			if !p.Covered {
				cnt += "（log 不完整，可能更多）"
			}
		}
		w("  %s：%s", p.UID, cnt)
		w("    log 收集：%s", p.Coverage)
	}

	w("")
	w("時間線：")
	for _, e := range d.Timeline {
		w("  %s  %s", ts(e.At), e.What)
	}
	w("rollout 結果：由 measure.sh 記錄（rush-hour 沒有這項資料）")

	w("")
	if h := d.Host; h != nil && h.Samples > 0 {
		w("主機資源：CPU 最高 %.1f%%，可用記憶體最低 %.1f%%（%d 次取樣）", h.MaxCPUPct, h.MinMemAvailPct, h.Samples)
	} else if h != nil && h.Err != "" {
		w("主機資源：取不到（%s）", h.Err)
	} else {
		w("主機資源：沒有取樣")
	}
	w("rush-hour 排程延遲：最大 %d ms，p99 %d ms", d.SchedLagMaxMs, d.SchedLagP99Ms)
	if d.ResourceTight {
		w("同時觀察到資源吃緊（門檻暫定：CPU ≥ %.0f%%、可用記憶體 ≤ %.0f%%、排程延遲 p99 ≥ %d ms）；這不是原因判定", cpuTightPct, memTightPct, schedLagTightMs)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// WriteScorecard writes DIR/scorecard.txt and DIR/scorecard.json.
func WriteScorecard(dir string, sc Scorecard, verbose bool) error {
	j, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(dir, "scorecard.json"), append(j, '\n')); err != nil {
		return err
	}
	return writeFileSync(filepath.Join(dir, "scorecard.txt"), []byte(sc.Text(verbose)))
}

func writeFileSync(path string, data []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	return errors.Join(werr, serr, cerr)
}

// ---------------------------------------------------------------- summary

// Summarize reads DIR/scorecard.json (or the file itself) for each path and
// prints every run, then the median and range per item over the runs where
// the item is fully known. Invalid runs and unknown values are listed but
// never counted.
func Summarize(paths []string) (string, error) {
	var cards []Scorecard
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			p = filepath.Join(p, "scorecard.json")
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		var sc Scorecard
		if err := json.Unmarshal(b, &sc); err != nil {
			return "", fmt.Errorf("%s: %w", p, err)
		}
		cards = append(cards, sc)
	}
	type row struct {
		label, unit string
		get         func(Items) (Item, bool)
		dup         bool
	}
	rows := []row{
		{"訂單", "張", func(i Items) (Item, bool) { return i.Orders, true }, false},
		{"第一次送出沒有成功回應", "張", func(i Items) (Item, bool) { return i.FirstTryFailed, true }, false},
		{"重試後仍沒有成功回應", "張", func(i Items) (Item, bool) { return i.FailedAfterRetry, true }, false},
		{"重複訂單（client_ref 數）", "", func(i Items) (Item, bool) { return i.DuplicateRefs, true }, true},
		{"多出的訂單張數", "張", func(i Items) (Item, bool) { return i.DuplicateExtra, true }, true},
	}
	boardNames := map[string]bool{}
	for _, c := range cards {
		for n := range c.Items.Missing {
			boardNames[n] = true
		}
	}
	for _, n := range sortedKeys(boardNames) {
		n := n
		rows = append(rows, row{"漏號 " + n, "個叫號", func(i Items) (Item, bool) { v, ok := i.Missing[n]; return v, ok }, false})
	}
	format := func(r row, v Item) string {
		switch {
		case r.dup:
			return dupText(v, r.unit)
		case r.unit == "個叫號":
			return v.count().StringUnit(r.unit)
		default:
			return withUnit(v, r.unit)
		}
	}

	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("彙整表（%d 次）", len(cards))
	for i, c := range cards {
		if c.Invalid {
			w("第 %d 次 %s：本次量測無效（%s），不列入", i+1, c.Run, c.InvalidReason)
			continue
		}
		var parts []string
		for _, r := range rows {
			if v, ok := r.get(c.Items); ok {
				parts = append(parts, r.label+" "+format(r, v))
			}
		}
		w("第 %d 次 %s：%s", i+1, c.Run, strings.Join(parts, "；"))
	}
	w("")
	for _, r := range rows {
		var vals []int
		skipped := 0
		for _, c := range cards {
			if c.Invalid {
				continue
			}
			v, ok := r.get(c.Items)
			if !ok || v.Unknown > 0 {
				skipped++
				continue
			}
			vals = append(vals, v.Confirmed)
		}
		note := ""
		if skipped > 0 {
			note = fmt.Sprintf("（另有 %d 次含未知，未列入）", skipped)
		}
		if len(vals) == 0 {
			w("%s：無法計算%s", r.label, note)
			continue
		}
		sort.Ints(vals)
		w("%s：中位數 %s，範圍 %d～%d%s", r.label, median(vals), vals[0], vals[len(vals)-1], note)
	}
	return b.String(), nil
}

func median(sorted []int) string {
	n := len(sorted)
	if n%2 == 1 {
		return fmt.Sprintf("%d", sorted[n/2])
	}
	s := sorted[n/2-1] + sorted[n/2]
	if s%2 == 0 {
		return fmt.Sprintf("%d", s/2)
	}
	return fmt.Sprintf("%.1f", float64(s)/2)
}
