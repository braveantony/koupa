// Package winddown runs Rush Hour's wind-down (plan A9 steps 1-7), reads the
// evidence (Rush Hour's own JSONL, App logs, the ingress access log) and
// builds the scorecard.
package winddown

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"koupa/rush-hour/record"
	"koupa/rush-hour/verdict"
)

// Paths names the evidence files. An empty AppLogs or Ingress means "not
// collected"; every dependent verdict is then unknown.
type Paths struct {
	Attempts string // DIR/attempts.jsonl
	Boards   string // DIR/boards.jsonl
	AppLogs  string // --app-logs DIR: <pod-uid>.log (+ <pod-uid>.meta.json)
	Ingress  string // --ingress-log FILE
}

// Evidence is everything read back from disk for one run.
type Evidence struct {
	Attempts    []record.Attempt
	AttemptsBad int   // lines that did not parse
	AttemptsErr error // file unreadable
	Boards      []record.BoardEvent
	BoardsBad   int
	BoardsErr   error
	App         *AppLogs
	Ingress     *IngressLog
}

// LoadEvidence reads all evidence files. Missing optional sources are
// reported inside App / Ingress (Available=false), never as an error.
func LoadEvidence(p Paths) Evidence {
	var ev Evidence
	ev.Attempts, ev.AttemptsBad, ev.AttemptsErr = readJSONL[record.Attempt](p.Attempts)
	ev.Boards, ev.BoardsBad, ev.BoardsErr = readJSONL[record.BoardEvent](p.Boards)
	if p.AppLogs != "" {
		ev.App = ReadAppLogs(p.AppLogs)
	} else {
		ev.App = &AppLogs{Err: "沒有指定 --app-logs"}
	}
	ips := map[string]string{}
	for uid, pod := range ev.App.Pods {
		if pod.Meta != nil && pod.Meta.PodIP != "" {
			ips[pod.Meta.PodIP] = uid
		}
	}
	if p.Ingress != "" {
		ev.Ingress = ReadIngressLog(p.Ingress, ips)
	} else {
		ev.Ingress = &IngressLog{Err: "沒有指定 --ingress-log"}
	}
	return ev
}

// readJSONL parses one JSON value per line. A final line without a newline
// is still parsed (the writer always ends lines, so a torn tail shows up as
// a bad line).
func readJSONL[T any](path string) ([]T, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	var out []T
	bad := 0
	for _, l := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		var v T
		if json.Unmarshal(l, &v) != nil {
			bad++
			continue
		}
		out = append(out, v)
	}
	return out, bad, nil
}

// jsonPart strips any prefix before the first '{' (kubectl --timestamps,
// Envoy/container runtime prefixes). ok=false when the line has no '{'.
func jsonPart(line []byte) ([]byte, bool) {
	i := bytes.IndexByte(line, '{')
	if i < 0 {
		return nil, false
	}
	return line[i:], true
}

func scanLines(path string, fn func(line []byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		fn(sc.Bytes())
	}
	return sc.Err()
}

func parseTime(s *string) *time.Time {
	if s == nil || *s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return nil
	}
	return &t
}

// ---------------------------------------------------------------- ingress

// IngressLog is the parsed ingress access log.
//
// Line contract (Envoy access log, Task 14): one JSON object per line,
// optionally preceded by a non-JSON prefix; keys start_time, path, method,
// status, upstream_host, response_flags, response_code_details,
// upstream_attempts, duration_ms, attempt (= X-Koupa-Attempt). A line
// without attempt (or "-") is ignored. A logged line means the attempt
// finished at the ingress.
type IngressLog struct {
	Available bool
	Err       string
	// Recs is nil when the log is not available: every lookup is nil.
	Recs  map[string]*verdict.IngressRecord
	Lines map[string]IngressLine // raw line per attempt (last one), for diagnostics
	Bad   int                    // lines with '{' that did not parse
	Dups  []string               // attempts logged more than once
}

// IngressLine is one access-log line.
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
	Attempt             string          `json:"attempt"`
}

// rawInt reads a JSON number or numeric string; ok=false for null, "-", etc.
func rawInt(r json.RawMessage) (int, bool) {
	if len(r) == 0 {
		return 0, false
	}
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

// hasFlag reports whether Envoy's comma-separated response_flags has f.
func hasFlag(flags, f string) bool {
	for _, x := range strings.Split(flags, ",") {
		if strings.TrimSpace(x) == f {
			return true
		}
	}
	return false
}

// ReadIngressLog parses the access log. podByIP maps a Pod IP to its UID
// (from the meta files) so the upstream address can name a Pod.
func ReadIngressLog(path string, podByIP map[string]string) *IngressLog {
	out := &IngressLog{Recs: map[string]*verdict.IngressRecord{}, Lines: map[string]IngressLine{}}
	count := map[string]int{}
	err := scanLines(path, func(line []byte) {
		js, ok := jsonPart(line)
		if !ok {
			return
		}
		var l IngressLine
		if json.Unmarshal(js, &l) != nil {
			out.Bad++
			return
		}
		if l.Attempt == "" || l.Attempt == "-" {
			return
		}
		count[l.Attempt]++
		out.Lines[l.Attempt] = l
		rec := &verdict.IngressRecord{Finished: true}
		if n, ok := rawInt(l.UpstreamAttempts); ok {
			// Envoy logs 0 both for "no upstream chosen" and for "not
			// recorded"; only UH/NR prove that nothing was forwarded.
			if n > 0 || hasFlag(l.ResponseFlags, "UH") || hasFlag(l.ResponseFlags, "NR") {
				v := n
				rec.UpstreamAttempts = &v
			}
		}
		if host, _, err := net.SplitHostPort(l.UpstreamHost); err == nil {
			rec.UpstreamPod = podByIP[host]
		}
		st := l.StartTime
		rec.IngressStart = parseTime(&st)
		out.Recs[l.Attempt] = rec
	})
	if err != nil {
		return &IngressLog{Err: err.Error()}
	}
	for a, n := range count {
		if n > 1 {
			// One downstream request per attempt id: two lines contradict.
			out.Recs[a].UpstreamAttempts = nil
			out.Dups = append(out.Dups, a)
		}
	}
	sort.Strings(out.Dups)
	out.Available = true
	return out
}

// ---------------------------------------------------------------- app logs

// PodMeta is <dir>/<pod-uid>.meta.json, written by measure.sh (Task 20).
// complete: the log stream was collected to the end (Pod exited and the
// collector drained it). exited_at: kubelet finishedAt; term_start: the
// estimated termination start (A11). pod_ip and term_start_precision_ms
// are optional: the first maps ingress upstream_host to the Pod, the second
// is the timing margin for Cause (absent = unknown precision).
type PodMeta struct {
	PodUID      string  `json:"pod_uid"`
	PodName     string  `json:"pod_name"`
	Complete    bool    `json:"complete"`
	ExitedAt    *string `json:"exited_at"`
	TermStart   *string `json:"term_start"`
	PodIP       string  `json:"pod_ip,omitempty"`
	PrecisionMs *int64  `json:"term_start_precision_ms,omitempty"`
}

// appLine is one App log line (applog: event = slog message).
type appLine struct {
	TS               string  `json:"ts"`
	Event            string  `json:"event"`
	PodUID           string  `json:"pod_uid"`
	PodName          string  `json:"pod_name"`
	Attempt          *string `json:"attempt"`
	Path             string  `json:"path"`
	Status           *int    `json:"status"`
	WriteErr         *string `json:"write_err"`
	FlushErr         *string `json:"flush_err"`
	CtxCanceled      *bool   `json:"ctx_canceled"`
	TxOutcome        *string `json:"tx_outcome"`
	CommitSentAt     *string `json:"commit_sent_at"`
	OrderID          *int64  `json:"order_id"`
	ReadySeq         *int64  `json:"ready_seq"`
	NewlyAssigned    *bool   `json:"newly_assigned"`
	GracefulShutdown *bool   `json:"graceful_shutdown"`
	// Test switches: test_hooks is a bool, test_mutation_* are rates
	// (numbers); any non-zero / true value means "on".
	TestHooks    json.RawMessage `json:"test_hooks"`
	MutDropReady json.RawMessage `json:"test_mutation_drop_ready"`
	MutDupOrder  json.RawMessage `json:"test_mutation_duplicate_order"`
}

// AppRecv is a request_received line.
type AppRecv struct {
	Pod string
	At  time.Time
}

// AppEndLine is a request_end line.
type AppEndLine struct {
	Pod           string
	At            time.Time
	Status        *int
	End           verdict.AppEnd
	OrderID       *int64
	ReadySeq      *int64
	NewlyAssigned *bool
}

// PodLog summarises one Pod's log file.
type PodLog struct {
	UID, Name   string
	Startup     bool
	Graceful    *bool // from the startup line
	SigtermAt   *time.Time
	ExitAt      *time.Time
	Bad         int // lines with '{' that did not parse or name another Pod
	Meta        *PodMeta
	MetaErr     string
	Covered     bool
	PostSigterm int // request_received lines after the sigterm line
	TestFlags   []string
}

// AppLogs is the parsed --app-logs directory.
type AppLogs struct {
	Available     bool
	Err           string
	Pods          map[string]*PodLog
	Received      map[string][]AppRecv
	Ends          map[string][]AppEndLine
	Contradictory map[string]bool // attempt -> records disagree
}

// ReadAppLogs reads every <pod-uid>.log in dir and its optional meta file.
//
// Coverage (A11, until Task 20 refines it): a Pod's log is covered when it
// has a startup line, every JSON line parsed, and either its final marker
// is present (exit when graceful_shutdown, otherwise sigterm) or the meta
// file says complete. For covered Pods the contradiction check runs:
// request_end without request_received, or more than one of either line,
// for the same attempt.
func ReadAppLogs(dir string) *AppLogs {
	out := &AppLogs{
		Pods: map[string]*PodLog{}, Received: map[string][]AppRecv{},
		Ends: map[string][]AppEndLine{}, Contradictory: map[string]bool{},
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil {
		out.Err = err.Error()
		return out
	}
	if _, err := os.Stat(dir); err != nil {
		out.Err = err.Error()
		return out
	}
	sort.Strings(files)
	for _, f := range files {
		uid := strings.TrimSuffix(filepath.Base(f), ".log")
		p := &PodLog{UID: uid}
		out.Pods[uid] = p
		if b, err := os.ReadFile(filepath.Join(dir, uid+".meta.json")); err == nil {
			var m PodMeta
			if err := json.Unmarshal(b, &m); err != nil {
				p.MetaErr = err.Error()
			} else {
				p.Meta = &m
				if p.Name == "" {
					p.Name = m.PodName
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			p.MetaErr = err.Error()
		}
		var recvAfter []time.Time
		err := scanLines(f, func(line []byte) {
			js, ok := jsonPart(line)
			if !ok {
				return
			}
			var l appLine
			if json.Unmarshal(js, &l) != nil || (l.PodUID != "" && l.PodUID != uid) {
				p.Bad++
				return
			}
			if l.PodName != "" {
				p.Name = l.PodName
			}
			ts := parseTime(&l.TS)
			switch l.Event {
			case "startup":
				p.Startup = true
				p.Graceful = l.GracefulShutdown
				for name, v := range map[string]json.RawMessage{"TEST_HOOKS": l.TestHooks, "TEST_MUTATION drop_ready": l.MutDropReady, "TEST_MUTATION duplicate_order": l.MutDupOrder} {
					if switchOn(v) {
						p.TestFlags = append(p.TestFlags, name)
					}
				}
				sort.Strings(p.TestFlags)
			case "sigterm":
				if p.SigtermAt == nil {
					p.SigtermAt = ts
				}
			case "exit":
				p.ExitAt = ts
			case "request_received":
				if l.Attempt == nil || *l.Attempt == "" || ts == nil {
					return
				}
				out.Received[*l.Attempt] = append(out.Received[*l.Attempt], AppRecv{Pod: uid, At: *ts})
				recvAfter = append(recvAfter, *ts)
			case "request_end":
				if l.Attempt == nil || *l.Attempt == "" {
					return
				}
				e := AppEndLine{Pod: uid, Status: l.Status, OrderID: l.OrderID, ReadySeq: l.ReadySeq, NewlyAssigned: l.NewlyAssigned}
				if ts != nil {
					e.At = *ts
				}
				if l.TxOutcome != nil {
					e.End.TxOutcome = *l.TxOutcome
				}
				e.End.CommitSentAt = parseTime(l.CommitSentAt)
				e.End.WriteErr = nonEmpty(l.WriteErr)
				e.End.FlushErr = nonEmpty(l.FlushErr)
				e.End.CtxCanceled = l.CtxCanceled
				out.Ends[*l.Attempt] = append(out.Ends[*l.Attempt], e)
			}
		})
		if err != nil {
			p.Bad++
			p.MetaErr = strings.TrimSpace(p.MetaErr + " log: " + err.Error())
		}
		if p.SigtermAt != nil {
			for _, t := range recvAfter {
				if t.After(*p.SigtermAt) {
					p.PostSigterm++
				}
			}
		}
		marker := false
		if p.Graceful != nil && *p.Graceful {
			marker = p.ExitAt != nil
		} else if p.Graceful != nil {
			marker = p.SigtermAt != nil
		}
		p.Covered = p.Startup && p.Bad == 0 && (marker || (p.Meta != nil && p.Meta.Complete))
	}
	// Contradiction check, for attempts on covered Pods.
	for a, ends := range out.Ends {
		recv := out.Received[a]
		if len(recv) == 0 || len(ends) > 1 || len(recv) > 1 || ends[0].Pod != recv[0].Pod {
			out.Contradictory[a] = true
		}
	}
	for a, recv := range out.Received {
		if len(recv) > 1 {
			out.Contradictory[a] = true
		}
	}
	out.Available = true
	return out
}

// switchOn reports whether a logged test switch is on: true, a non-zero
// number, or a string other than "", "0" and "false".
func switchOn(r json.RawMessage) bool {
	var v any
	if len(r) == 0 || json.Unmarshal(r, &v) != nil {
		return false
	}
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != "" && x != "0" && x != "false"
	}
	return false
}

// nonEmpty turns the App's error string into "was there an error"; nil when
// the field was not logged.
func nonEmpty(s *string) *bool {
	if s == nil {
		return nil
	}
	v := *s != ""
	return &v
}

// ---------------------------------------------------------------- assembly

// exitPrecision: kubelet's finishedAt has second precision, so the real
// exit may be up to 1s later than recorded. Settlement uses the later bound.
const exitPrecision = time.Second

// assemble builds the verdict input for one attempt record. dbReadyAttempt
// maps order_id to the database's ready_attempt (nil when not assigned or
// the audit failed); it feeds the DB-vs-log contradiction check (A10).
func assemble(a record.Attempt, ev Evidence, noIngress bool, dbReady map[int64]*string) verdict.AttemptEvidence {
	e := verdict.AttemptEvidence{
		FullSuccess: a.FullResponse && a.Status != nil && *a.Status/100 == 2,
	}
	if noIngress {
		one := 1
		e.Ingress = &verdict.IngressRecord{Finished: true, UpstreamAttempts: &one}
	} else if ev.Ingress != nil && ev.Ingress.Available {
		e.Ingress = ev.Ingress.Recs[a.Attempt]
	}
	app := ev.App
	if app == nil || !app.Available {
		return e
	}
	recv := app.Received[a.Attempt]
	ends := app.Ends[a.Attempt]
	pod := a.PodUID
	if len(recv) > 0 {
		e.AppReceived = true
		t := recv[0].At
		e.AppReceivedAt = &t
		pod = recv[0].Pod
	} else if len(ends) > 0 {
		pod = ends[0].Pod
	} else if pod == "" && e.Ingress != nil {
		pod = e.Ingress.UpstreamPod
	}
	if len(ends) > 0 {
		end := ends[0].End
		e.AppEnd = &end
	}
	e.Contradictory = app.Contradictory[a.Attempt]
	// DB vs log for ready attempts: ready_attempt is the final word.
	if a.Kind == record.KindReady && len(ends) == 1 && ends[0].OrderID != nil && dbReady != nil {
		end := ends[0]
		dbAtt, has := dbReady[*end.OrderID]
		assigned := has && dbAtt != nil && *dbAtt == a.Attempt
		if end.End.TxOutcome == verdict.TxCommitted && end.NewlyAssigned != nil && *end.NewlyAssigned != assigned {
			e.Contradictory = true
		}
	}
	p := app.Pods[pod]
	if p != nil {
		e.LogCovered = p.Covered
		if p.Meta != nil {
			if t := parseTime(p.Meta.ExitedAt); t != nil {
				x := t.Add(exitPrecision)
				e.PodExitedAt = &x
			}
		}
	}
	// Termination start of the upstream Pod (for Cause).
	tp := p
	if e.Ingress != nil && e.Ingress.UpstreamPod != "" {
		tp = app.Pods[e.Ingress.UpstreamPod]
	}
	if tp != nil && tp.Meta != nil {
		e.PodTermStart = parseTime(tp.Meta.TermStart)
		if tp.Meta.PrecisionMs != nil {
			d := time.Duration(*tp.Meta.PrecisionMs) * time.Millisecond
			e.Precision = &d
		}
	}
	return e
}

func (p *PodLog) describe() string {
	var parts []string
	if !p.Startup {
		parts = append(parts, "沒有 startup")
	}
	if p.Bad > 0 {
		parts = append(parts, fmt.Sprintf("%d 行無法解析", p.Bad))
	}
	if p.Covered {
		return "完整"
	}
	if p.Startup && p.Bad == 0 {
		parts = append(parts, "沒有結束標記，也沒有 complete 的 meta")
	}
	return "不完整（" + strings.Join(parts, "、") + "）"
}
