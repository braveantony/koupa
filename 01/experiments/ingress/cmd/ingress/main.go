// Command ingress is the client side of experiments/ingress/run.sh: the A10
// must-tests for ingress retries and late arrival, through a real Envoy.
//
//	Rush Hour (this command) -> Envoy -> testproxy.HTTP (this command) -> App
//
// It starts the proxy in front of the App, writes the proxy address to
// --proxy-addr-file (run.sh then starts Envoy pointing at it), waits for
// Envoy, and runs each case: arm the proxy, send exactly one order attempt
// through Envoy with Rush Hour's own client (load.Customer), wait until the
// ingress has logged the attempt, then compute the scorecard with Rush
// Hour's wind-down code (winddown.LoadEvidence + winddown.Evaluate, i.e.
// verdict.Settle and verdict.State) before releasing what the proxy holds:
// the late request arrives only after the wind-down has judged. After the
// release it audits again to show the late order exists.
//
// One JSON object per case on stdout; exit status 1 when a case could not
// be run (the expected verdicts are checked by run.sh).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"koupa/internal/wire"
	"koupa/rush-hour/load"
	"koupa/rush-hour/record"
	"koupa/rush-hour/winddown"
	"koupa/testproxy"
)

type harness struct {
	app, envoy         string // base URLs
	accessLog, appLogs string
	podUID, podName    string
	collectStart       string
	out, prefix        string
	timeout, txTimeout time.Duration
	p                  *testproxy.HTTP
	logf               func(format string, a ...any)
}

// tcase is one A10 must-test. arm sets the proxy rules for the attempt
// (matched by X-Koupa-Attempt) and returns what releases them after the
// scorecard. evalEnds / finalEnds: App request_end lines for the attempt
// expected before the scorecard and after the release.
type tcase struct {
	name, short         string
	evalEnds, finalEnds int
	arm                 func(p *testproxy.HTTP, match func(*http.Request) bool) (release func())
}

var cases = []tcase{
	{
		// 延遲抵達: the only upstream attempt is held past the client
		// timeout and the wind-down.
		name: "late-arrival", short: "late", evalEnds: 0, finalEnds: 1,
		arm: func(p *testproxy.HTTP, m func(*http.Request) bool) func() { return p.HoldNext(m) },
	},
	{
		// 入口重試且已經回應成功: try 1 reaches the proxy, which resets
		// Envoy's connection and keeps the request; Envoy retries, try 2
		// succeeds; try 1 commits after the wind-down (IDEMPOTENCY_KEY=0).
		name: "retry-success", short: "rsucc", evalEnds: 1, finalEnds: 2,
		arm: func(p *testproxy.HTTP, m func(*http.Request) bool) func() { return p.DropNext(m, nil) },
	},
	{
		// 入口重試且其中一次已在 App 結束, no full response: try 2 ends
		// normally in the App but its response is held past the client
		// timeout.
		name: "retry-ended-no-response", short: "rnoresp", evalEnds: 1, finalEnds: 2,
		arm: func(p *testproxy.HTTP, m func(*http.Request) bool) func() {
			r1 := p.DropNext(m, nil)
			r2 := p.HoldResponseNext(m)
			return func() { r1(); r2() }
		},
	},
	{
		// Same, full 5xx: try 2 ends normally in the App, then the proxy
		// resets Envoy's connection instead of relaying; retries are used
		// up, so Envoy answers with its own 503.
		name: "retry-ended-error", short: "rerr", evalEnds: 1, finalEnds: 2,
		arm: func(p *testproxy.HTTP, m func(*http.Request) bool) func() {
			r := p.DropNext(m, nil)
			p.CutResponseNext(m)
			return r
		},
	},
	{
		// 入口重試且最終旗標是 UF: try 1 is held after Envoy forwarded it;
		// the proxy stops listening before resetting, so the retry's
		// connect is refused (UF) and Envoy answers with a local 503.
		name: "retry-uf", short: "ruf", evalEnds: 0, finalEnds: 1,
		arm: func(p *testproxy.HTTP, m func(*http.Request) bool) func() {
			var mu sync.Mutex
			var resume func()
			r := p.DropNext(m, func() {
				mu.Lock()
				resume = p.Refuse()
				mu.Unlock()
			})
			return func() {
				mu.Lock()
				if resume != nil {
					resume()
				}
				mu.Unlock()
				r()
			}
		},
	},
}

// result is one case's line on stdout.
type result struct {
	Case        string         `json:"case"`
	Run         string         `json:"run"`
	Attempt     string         `json:"attempt"`
	ClientRef   string         `json:"client_ref"`
	Error       string         `json:"error"`
	Ingress     map[string]any `json:"ingress"`
	IngressLine string         `json:"ingress_line"`
	// Rush Hour's attempt record.
	ClientStatus *int   `json:"client_status"`
	ClientErr    string `json:"client_err"`
	FullSuccess  bool   `json:"full_success"`
	// Scorecard (winddown.Evaluate) at EvaluatedAt.
	EvaluatedAt    time.Time     `json:"evaluated_at"`
	Invalid        bool          `json:"invalid"`
	InvalidReasons []string      `json:"invalid_reasons"`
	Settled        bool          `json:"settled"`
	SettleReason   string        `json:"settle_reason"`
	State          string        `json:"state"` // "" when the attempt is not a failed one
	Cause          string        `json:"cause"`
	HasUnknown     bool          `json:"has_unknown"`
	DuplicateRefs  winddown.Item `json:"duplicate_refs"`
	DuplicateExtra winddown.Item `json:"duplicate_extra"`
	OrdersAtEval   int           `json:"orders_at_eval"`
	AppRecvAtEval  int           `json:"app_received_at_eval"`
	AppEndsAtEval  int           `json:"app_ends_at_eval"`
	ReleasedAt     time.Time     `json:"released_at"`
	AppEndsFinal   int           `json:"app_ends_final"`
	OrdersAfter    int           `json:"orders_after_release"`
	AppLines       []string      `json:"app_lines"`
	Scorecard      string        `json:"scorecard"`
}

func main() {
	h := &harness{}
	var addrFile, appArg string
	flag.StringVar(&appArg, "app", "", "App base URL (direct, for /api/audit), e.g. http://127.0.0.1:18080")
	flag.StringVar(&h.envoy, "envoy", "", "Envoy listener base URL")
	flag.StringVar(&h.accessLog, "access-log", "", "Envoy access log file")
	flag.StringVar(&h.appLogs, "app-logs", "", "directory holding <pod-uid>.log (App stdout)")
	flag.StringVar(&h.podUID, "pod-uid", "", "the App's POD_UID")
	flag.StringVar(&h.podName, "pod-name", "", "the App's POD_NAME")
	flag.StringVar(&h.collectStart, "collect-start", "", "RFC 3339 time the App log capture started")
	flag.StringVar(&h.out, "out", "", "directory for one Rush Hour run directory per case")
	flag.StringVar(&addrFile, "proxy-addr-file", "", "where to write the proxy address")
	flag.DurationVar(&h.timeout, "timeout", 5*time.Second, "Rush Hour per-attempt client timeout")
	flag.DurationVar(&h.txTimeout, "tx-timeout", 60*time.Second, "database transaction_timeout")
	flag.Parse()
	if appArg == "" || h.envoy == "" || h.accessLog == "" || h.appLogs == "" || h.podUID == "" ||
		h.collectStart == "" || h.out == "" || addrFile == "" {
		fmt.Fprintln(os.Stderr, "ingress: --app, --envoy, --access-log, --app-logs, --pod-uid, --collect-start, --out and --proxy-addr-file are required")
		os.Exit(2)
	}
	h.app = strings.TrimRight(appArg, "/")
	h.envoy = strings.TrimRight(h.envoy, "/")
	h.logf = func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "%s ingress: %s\n", time.Now().UTC().Format("15:04:05.000"), fmt.Sprintf(format, a...))
	}
	u, err := url.Parse(h.app)
	if err != nil || u.Host == "" {
		fmt.Fprintln(os.Stderr, "ingress: bad --app")
		os.Exit(2)
	}
	h.p = testproxy.NewHTTP(u.Host)
	defer h.p.Close()
	if err := writeAtomic(addrFile, []byte(h.p.Addr()+"\n")); err != nil {
		fmt.Fprintln(os.Stderr, "ingress:", err)
		os.Exit(2)
	}
	h.logf("proxy %s -> %s; waiting for Envoy at %s", h.p.Addr(), u.Host, h.envoy)
	if err := h.warmup(60 * time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "ingress: Envoy not ready:", err)
		os.Exit(2)
	}
	h.prefix = "ing" + time.Now().UTC().Format("150405")
	enc := json.NewEncoder(os.Stdout)
	code := 0
	for _, c := range cases {
		res := h.runCase(c)
		if res.Error != "" {
			h.logf("%s: ERROR %s", c.name, res.Error)
			code = 1
		}
		if err := enc.Encode(res); err != nil {
			code = 1
		}
	}
	os.Exit(code)
}

// warmup waits for a 200 from /healthz through Envoy. Between cases it
// also resets Envoy's consecutive-failure count for outlier detection.
func (h *harness) warmup(limit time.Duration) error {
	c := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	deadline := time.Now().Add(limit)
	var last error
	for time.Now().Before(deadline) {
		resp, err := c.Get(h.envoy + "/healthz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		last = err
		time.Sleep(200 * time.Millisecond)
	}
	return last
}

func (h *harness) runCase(c tcase) (res result) {
	run := h.prefix + "-" + c.short
	res = result{Case: c.name, Run: run, ClientRef: run + "-c1", Attempt: wire.NewAttemptID(run, 1, 1)}
	fail := func(format string, a ...any) result {
		res.Error = fmt.Sprintf(format, a...)
		return res
	}
	h.logf("=== %s (attempt %s)", c.name, res.Attempt)
	if err := h.warmup(10 * time.Second); err != nil {
		return fail("warm-up through Envoy: %v", err)
	}
	att := res.Attempt
	match := func(r *http.Request) bool { return r.Header.Get(wire.HeaderAttempt) == att }
	release := c.arm(h.p, match)
	var relOnce sync.Once
	doRelease := func() { relOnce.Do(release) }
	defer doRelease() // never leave a held request behind for the next case

	dir := filepath.Join(h.out, c.name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail("%v", err)
	}
	paths := winddown.Paths{
		Attempts: filepath.Join(dir, "attempts.jsonl"), Boards: filepath.Join(dir, "boards.jsonl"),
		AppLogs: h.appLogs, Ingress: h.accessLog,
	}
	if err := os.WriteFile(paths.Boards, nil, 0o644); err != nil {
		return fail("%v", err)
	}
	af, err := os.OpenFile(paths.Attempts, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fail("%v", err)
	}
	w := record.NewWriter(af)
	cust := &load.Customer{
		Client: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
		Base:   h.envoy, Run: run, Timeout: h.timeout, Retries: 0, W: w, MenuIDs: []int{1}, Seed: 1,
	}
	h.logf("send order through Envoy")
	perr := cust.Place(context.Background(), 1, time.Now())
	if err := errors.Join(perr, af.Close()); err != nil {
		return fail("attempt record: %v", err)
	}

	line, raw, err := h.waitIngress(att, 15*time.Second)
	if err != nil {
		return fail("%v", err)
	}
	res.Ingress, res.IngressLine = line, raw
	h.logf("ingress: %s", raw)
	recv, ends, err := h.waitEnds(att, c.evalEnds, 15*time.Second)
	if err != nil {
		return fail("before the scorecard: %v", err)
	}
	res.AppRecvAtEval, res.AppEndsAtEval = recv, ends

	// The wind-down's judgement, before anything is released.
	now := time.Now()
	if err := h.writeMeta(now); err != nil {
		return fail("meta: %v", err)
	}
	audit, err := h.audit(run)
	if err != nil {
		return fail("audit: %v", err)
	}
	facts := winddown.Facts{
		Run: run, TxTimeoutMs: h.txTimeout.Milliseconds(), EvaluatedAt: now, Retries: 0,
		Audit: audit, CaughtUp: map[string]bool{}, IngressPath: h.accessLog, AppLogsDir: h.appLogs,
	}
	ev := winddown.LoadEvidence(paths)
	if len(ev.Attempts) != 1 || ev.Attempts[0].Attempt != att {
		return fail("want exactly one attempt record %s, got %d", att, len(ev.Attempts))
	}
	a := ev.Attempts[0]
	res.ClientStatus, res.ClientErr = a.Status, a.ErrKind
	res.FullSuccess = a.FullResponse && a.Status != nil && *a.Status/100 == 2
	sc := winddown.Evaluate(facts, ev)
	if err := winddown.WriteScorecard(dir, sc, true); err != nil {
		return fail("scorecard: %v", err)
	}
	res.Scorecard = filepath.Join(dir, "scorecard.txt")
	res.EvaluatedAt = now
	res.Invalid, res.InvalidReasons = sc.Invalid, sc.InvalidReasons
	res.HasUnknown = sc.HasUnknown
	res.DuplicateRefs, res.DuplicateExtra = sc.Items.DuplicateRefs, sc.Items.DuplicateExtra
	res.OrdersAtEval = len(audit.Orders[res.ClientRef])
	res.Settled = true
	for _, u := range sc.Diag.Unsettled {
		if u.Attempt == att {
			res.Settled, res.SettleReason = false, u.Reason
		}
	}
	for _, f := range sc.Diag.Failed {
		if f.Attempt == att {
			res.State, res.Cause = string(f.State), string(f.Cause)
		}
	}
	h.logf("scorecard: settled=%v (%s) state=%q", res.Settled, res.SettleReason, res.State)

	// Wind-down done: now the held request may arrive.
	res.ReleasedAt = time.Now()
	doRelease()
	h.logf("released")
	_, ends, err = h.waitEnds(att, c.finalEnds, 20*time.Second)
	res.AppEndsFinal = ends
	if err != nil {
		return fail("after the release: %v", err)
	}
	after, err := h.audit(run)
	if err != nil {
		return fail("audit after the release: %v", err)
	}
	res.OrdersAfter = len(after.Orders[res.ClientRef])
	res.AppLines, _ = grepLines(filepath.Join(h.appLogs, h.podUID+".log"), att)
	return res
}

// waitIngress waits for the access-log line of attempt att; it returns the
// line parsed and raw.
func (h *harness) waitIngress(att string, limit time.Duration) (map[string]any, string, error) {
	deadline := time.Now().Add(limit)
	for {
		lines, err := grepLines(h.accessLog, att)
		var found []string
		var m map[string]any
		for _, l := range lines {
			var x map[string]any
			if json.Unmarshal([]byte(l), &x) == nil && x["attempt"] == att {
				found, m = append(found, l), x
			}
		}
		if len(found) > 1 {
			return nil, "", fmt.Errorf("access log has %d lines for %s", len(found), att)
		}
		if len(found) == 1 {
			return m, found[0], nil
		}
		if time.Now().After(deadline) {
			return nil, "", fmt.Errorf("no access-log line for %s within %v (err %v)", att, limit, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitEnds waits until the App log has want request_end lines for att and
// returns the received / request_end counts. For want 0 it watches for a
// second that none appears.
func (h *harness) waitEnds(att string, want int, limit time.Duration) (recv, ends int, err error) {
	if want == 0 {
		limit = time.Second
	}
	deadline := time.Now().Add(limit)
	for {
		logs := winddown.ReadAppLogs(h.appLogs)
		recv, ends = len(logs.Received[att]), len(logs.Ends[att])
		if want > 0 && ends >= want {
			if ends > want {
				return recv, ends, fmt.Errorf("App logged %d request_end lines for %s, want %d", ends, att, want)
			}
			return recv, ends, nil
		}
		if want == 0 && ends > 0 {
			return recv, ends, fmt.Errorf("App already ended %s (%d request_end lines), want none", att, ends)
		}
		if time.Now().After(deadline) {
			if want == 0 {
				return recv, ends, nil
			}
			return recv, ends, fmt.Errorf("App logged %d request_end lines for %s within %v, want %d", ends, att, limit, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// writeMeta writes the collector's meta for the App log: complete, from
// --collect-start up to end (the App runs for the whole experiment and
// its stdout goes straight to the file).
func (h *harness) writeMeta(end time.Time) error {
	e := end.UTC().Format(time.RFC3339Nano)
	cs := h.collectStart
	m := winddown.PodMeta{
		PodUID: h.podUID, PodName: h.podName, Complete: true, PodIP: "127.0.0.1",
		CollectStart: &cs, CollectEnd: &e,
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(h.appLogs, h.podUID+".meta.json"), append(b, '\n'))
}

// audit reads A9 step 5 for run directly from the App.
func (h *harness) audit(run string) (winddown.Audit, error) {
	var a winddown.Audit
	get := func(mode string, v any) error {
		c := &http.Client{Timeout: 10 * time.Second}
		resp, err := c.Get(h.app + "/api/audit?run=" + url.QueryEscape(run) + "&mode=" + mode)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: HTTP %d", mode, resp.StatusCode)
		}
		return json.NewDecoder(resp.Body).Decode(v)
	}
	var cur struct {
		Cursor *int64 `json:"cursor"`
	}
	if err := get("cursor", &cur); err != nil {
		return a, err
	}
	a.Cursor = cur.Cursor
	if err := get("orders", &a.Orders); err != nil {
		return a, err
	}
	if err := get("ready", &a.Ready); err != nil {
		return a, err
	}
	if a.Cursor == nil || a.Orders == nil || a.Ready == nil {
		return a, errors.New("incomplete audit response")
	}
	return a, nil
}

func grepLines(path, needle string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if strings.Contains(sc.Text(), needle) {
			out = append(out, sc.Text())
		}
	}
	return out, sc.Err()
}

func writeAtomic(path string, b []byte) error {
	if err := os.WriteFile(path+".tmp", b, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}
