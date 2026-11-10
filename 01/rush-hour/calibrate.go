package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"koupa/rush-hour/calib"
	"koupa/rush-hour/record"
)

// calibSample is one terminated Pod of a calibrate run, as written to the
// --json report.
type calibSample struct {
	Run            string   `json:"run"`
	PodUID         string   `json:"pod_uid"`
	PodName        string   `json:"pod_name"`
	DeletionStart  string   `json:"deletion_start"`
	PreStopEnd     string   `json:"prestop_end"`
	Lower          *string  `json:"lower"`
	Upper          *string  `json:"upper"`
	UpperKind      string   `json:"upper_kind"`
	P              float64  `json:"p"`
	K              int      `json:"k"`
	N              int      `json:"n"`
	RunsP          *float64 `json:"runs_test_p"`
	RateP          *float64 `json:"rate_test_p"`
	UMinusDeletion *float64 `json:"u_minus_deletion_s"`
	// LowerSource: "ingress" when L is the access-log start_time of the
	// last probe the ingress sent to the Pod, "sent" when it is SentAt.
	LowerSource    string   `json:"lower_source"`
	LMinusDeletion *float64 `json:"l_minus_deletion_s"`
	// Last access-log line (any request, not only probes) whose upstream is
	// this Pod, minus the deletion start: a cross-reference only (it is a
	// lower bound, never used as U). nil without an ingress log.
	LastIngressAny *float64 `json:"last_ingress_any_minus_deletion_s"`
	Valid          bool     `json:"valid"`
	InvalidReason  string   `json:"invalid_reason"`
}

type calibReport struct {
	Model       string       `json:"model"`
	Config      calib.Config `json:"config"`
	PreStopS    float64      `json:"prestop_s"`
	Upper       string       `json:"upper_bound_method"`
	DirectNote  string       `json:"direct_upper_note"`
	FreshConn   bool         `json:"fresh_connections"`
	PerAttempt  bool         `json:"per_attempt_ingress"`
	IngressRuns int          `json:"runs_with_ingress_log"`
	// Probes with a response but no usable access-log line (no line, or
	// upstream_attempts != 1): their L candidate is SentAt.
	NoIngressLine int `json:"probes_without_ingress_line"`
	// Access-log lines with an upstream but upstream_attempts != 1 (retried
	// inside the ingress, or not recorded): the final upstream does not say
	// which Pod was selected when; excluded from every L.
	Ambiguous int `json:"ingress_lines_ambiguous"`
	// Attempt ids logged more than once: unusable for the probe join.
	DupAttempts int           `json:"ingress_duplicate_attempts"`
	IngressNote string        `json:"ingress_note"`
	Runs        []string      `json:"runs"`
	ProbesNoUID int           `json:"probes_without_pod_uid"`
	Samples     []calibSample `json:"samples"`
	Valid       int           `json:"valid_samples"`
	CandidateS  *float64      `json:"candidate_s"`
	Note        string        `json:"note"`
}

const directNote = "no endpoint or configuration update records covering every ingress instance " +
	"are collected, so DirectUpper is unknown (nil): every upper bound is the model estimate (A11 step 4)"

// cmdCalibrate: rush-hour calibrate [--prestop 20s] [--json FILE] DIR...
// For every Pod with a term_start in DIR/app/<uid>.meta.json it runs
// calib.Estimate on DIR/probes.jsonl and prints the samples table and the
// candidate (max U - deletion over valid samples, no buffer).
func cmdCalibrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("calibrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	preStop := fs.Duration("prestop", 20*time.Second, "preStop sleep of the calibrate overlay")
	jsonOut := fs.String("json", "", "also write the report as JSON to FILE")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 || *preStop <= 0 {
		fmt.Fprintln(stderr, "usage: rush-hour calibrate [--prestop 20s] [--json FILE] DIR...")
		return 2
	}
	rep, err := calibrateDirs(fs.Args(), *preStop, calib.DefaultConfig())
	if err != nil {
		fmt.Fprintln(stderr, "rush-hour calibrate:", err)
		return 2
	}
	if *jsonOut != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err == nil {
			err = os.WriteFile(*jsonOut, append(b, '\n'), 0o644)
		}
		if err != nil {
			fmt.Fprintln(stderr, "rush-hour calibrate:", err)
			return 2
		}
	}
	writeCalibTable(stdout, rep)
	return 0
}

func calibrateDirs(dirs []string, preStop time.Duration, cfg calib.Config) (calibReport, error) {
	rep := calibReport{
		Model: "independent selection with the pre-deletion hit rate p; U = response of the k-th " +
			"consecutive miss after L, (1-p)^k <= MissProb (A11 step 4)",
		Config: cfg, PreStopS: preStop.Seconds(), Upper: calib.UpperModel, DirectNote: directNote,
		// load.Prober always uses a transport with keep-alives disabled.
		FreshConn: true, PerAttempt: false,
		Note: "candidate_s has no buffer; step 1 uses ceil(candidate_s) + buffer",
		IngressNote: "DIR/ingress.log present: IngressSelectedAt = access-log start_time of the probe " +
			"(upstream_attempts 1); start_time is truncated to 1 ms, so one earlier than SentAt by " +
			"less than 1 ms is clamped to SentAt; L also takes the Pod's latest selection by any request " +
			"with upstream_attempts 1 (lower_source ingress-any), and model misses are counted after L; " +
			"lines with upstream_attempts != 1 are ambiguous and excluded; " +
			"per_attempt_ingress is true only if every run has the log",
	}
	var all []calib.Sample
	for i, dir := range dirs {
		probes, noUID, err := readProbes(filepath.Join(dir, "probes.jsonl"))
		if err != nil {
			return rep, err
		}
		rep.ProbesNoUID += noUID
		ing, err := readCalibIngress(filepath.Join(dir, "ingress.log"))
		if err != nil {
			return rep, err
		}
		if ing != nil {
			rep.IngressRuns++
			rep.NoIngressLine += ing.join(probes)
			rep.Ambiguous += ing.ambiguous
			rep.DupAttempts += ing.dups
		}
		rep.PerAttempt = rep.IngressRuns == i+1
		cprobes := make([]calib.Probe, len(probes))
		for j, p := range probes {
			cprobes[j] = p.Probe
		}
		rep.Runs = append(rep.Runs, filepath.Base(dir))
		metas, err := filepath.Glob(filepath.Join(dir, "app", "*.meta.json"))
		if err != nil {
			return rep, err
		}
		sort.Strings(metas)
		for _, mp := range metas {
			var m struct {
				PodUID    string  `json:"pod_uid"`
				PodName   string  `json:"pod_name"`
				PodIP     string  `json:"pod_ip"`
				TermStart *string `json:"term_start"`
			}
			b, err := os.ReadFile(mp)
			if err != nil {
				return rep, err
			}
			if err := json.Unmarshal(b, &m); err != nil {
				return rep, fmt.Errorf("%s: %w", mp, err)
			}
			if m.TermStart == nil || *m.TermStart == "" {
				continue // not terminated during this run
			}
			del, err := time.Parse(time.RFC3339Nano, *m.TermStart)
			if err != nil {
				return rep, fmt.Errorf("%s: term_start: %w", mp, err)
			}
			in := calib.Input{Probes: cprobes, TargetPodUID: m.PodUID, DeletionStart: del,
				PreStopEnd: del.Add(preStop), DirectUpper: nil,
				PerAttemptIngress: ing != nil, FreshConnections: rep.FreshConn}
			var lastAny *time.Time
			if ing != nil && m.PodIP != "" {
				if last, ok := ing.lastByIP[m.PodIP]; ok {
					lastAny = &last
				}
			}
			in.LastSelection = lastAny
			s := calib.Estimate(in, cfg)
			all = append(all, s)
			cs := toCalibSample(filepath.Base(dir), m.PodUID, m.PodName, in, s)
			if lastAny != nil {
				d := lastAny.Sub(del).Seconds()
				cs.LastIngressAny = &d
			}
			rep.Samples = append(rep.Samples, cs)
			if s.Valid {
				rep.Valid++
			}
		}
	}
	if c, ok := calib.Candidate(all); ok {
		rep.CandidateS = &c
	}
	return rep, nil
}

func toCalibSample(run, uid, name string, in calib.Input, s calib.Sample) calibSample {
	rfc := func(t time.Time) *string {
		v := t.UTC().Format(time.RFC3339Nano)
		return &v
	}
	out := calibSample{Run: run, PodUID: uid, PodName: name,
		DeletionStart: *rfc(in.DeletionStart), PreStopEnd: *rfc(in.PreStopEnd),
		UpperKind: s.UpperKind, P: s.P, K: s.K, N: s.N, Valid: s.Valid, InvalidReason: s.InvalidReason}
	if !s.Lower.IsZero() {
		out.Lower = rfc(s.Lower)
		d := s.Lower.Sub(in.DeletionStart).Seconds()
		out.LMinusDeletion = &d
		out.LowerSource = "sent"
		if s.LowerFromSelection {
			out.LowerSource = "ingress-any"
		}
		for _, p := range in.Probes {
			if s.LowerFromSelection {
				break
			}
			if p.PodUID == uid && p.IngressSelectedAt != nil && p.IngressSelectedAt.Equal(s.Lower) {
				out.LowerSource = "ingress"
				break
			}
		}
	}
	if s.Upper != nil {
		out.Upper = rfc(*s.Upper)
		d := s.Upper.Sub(in.DeletionStart).Seconds()
		out.UMinusDeletion = &d
	}
	if s.RunsComputed {
		v := s.RunsP
		out.RunsP = &v
	}
	if s.RateComputed {
		v := s.RateP
		out.RateP = &v
	}
	return out
}

// readProbes reads the kind=probe records. noUID counts responses that came
// back with status 200 but without a Pod UID (A11: every probe response
// must carry one).
func readProbes(path string) (probes []probeRec, noUID int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var a record.Attempt
		if err := json.Unmarshal(sc.Bytes(), &a); err != nil {
			return nil, 0, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if a.Kind != record.KindProbe {
			continue
		}
		if a.PodUID == "" && a.Status != nil && *a.Status == 200 {
			noUID++
		}
		probes = append(probes, probeRec{Attempt: a.Attempt,
			Probe: calib.Probe{SentAt: a.SentAt, RespondedAt: a.RespondedAt, PodUID: a.PodUID}})
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	if len(probes) == 0 {
		return nil, 0, errors.New(path + ": no probe records")
	}
	return probes, noUID, nil
}

func writeCalibTable(w io.Writer, rep calibReport) {
	fmt.Fprintf(w, "preStop 校正（A11）：preStop %.0f 秒；上界取法：模型推估（%s）\n", rep.PreStopS,
		"沒有涵蓋所有入口實例的端點更新紀錄，直接證據未知")
	switch {
	case rep.IngressRuns == 0:
		fmt.Fprintln(w, "L 取法：探測的送出時間（沒有入口紀錄）")
	default:
		fmt.Fprintf(w, "L 取法：逐次入口紀錄（%d／%d 次 run 有 ingress.log；%d 個探測沒有可用的入口紀錄，改用送出時間）\n",
			rep.IngressRuns, len(rep.Runs), rep.NoIngressLine)
	}
	fmt.Fprintln(w, "last-any = 入口最後一次把任何請求送到這個 Pod 的開始時間 − 刪除請求時間（只是交叉參考的下界，不當成 U）")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "run\tpod\tdeletion\tL\tL-src\tU\tkind\tp\tk\tn\tL-del(s)\tU-del(s)\tlast-any(s)\tvalid/reason")
	short := func(s *string) string {
		if s == nil {
			return "-"
		}
		if t, err := time.Parse(time.RFC3339Nano, *s); err == nil {
			return t.UTC().Format("15:04:05.000")
		}
		return *s
	}
	for _, s := range rep.Samples {
		del := s.DeletionStart
		ud := "-"
		if s.UMinusDeletion != nil {
			ud = fmt.Sprintf("%.3f", *s.UMinusDeletion)
		}
		v := "valid"
		if !s.Valid {
			v = "invalid: " + s.InvalidReason
		}
		f3 := func(p *float64) string {
			if p == nil {
				return "-"
			}
			return fmt.Sprintf("%.3f", *p)
		}
		src := s.LowerSource
		if src == "" {
			src = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.3f\t%d\t%d\t%s\t%s\t%s\t%s\n", s.Run, s.PodName, short(&del),
			short(s.Lower), src, short(s.Upper), s.UpperKind, s.P, s.K, s.N, f3(s.LMinusDeletion), ud,
			f3(s.LastIngressAny), v)
	}
	tw.Flush()
	if rep.ProbesNoUID > 0 {
		fmt.Fprintf(w, "注意：%d 個探測回應沒有 Pod UID\n", rep.ProbesNoUID)
	}
	if rep.CandidateS != nil {
		fmt.Fprintf(w, "candidate: %.3f s（有效樣本 %d 個；不含緩衝；step 1 取 %d + 緩衝）\n",
			*rep.CandidateS, rep.Valid, int(math.Ceil(*rep.CandidateS)))
	} else {
		fmt.Fprintf(w, "no candidate: 有效樣本 %d 個（需要至少 5 個）\n", rep.Valid)
	}
}

// probeRec is a probe with its attempt id (X-Koupa-Attempt), the key into
// the ingress access log.
type probeRec struct {
	calib.Probe
	Attempt string
}

// calibIngress is the part of DIR/ingress.log calibrate uses.
type calibIngress struct {
	start     map[string]time.Time // attempt -> start_time, upstream_attempts == 1, logged once
	lastByIP  map[string]time.Time // upstream IP -> latest start_time, upstream_attempts == 1 only
	ambiguous int                  // lines with an upstream and upstream_attempts != 1
	dups      int                  // attempt ids logged more than once
}

// startTimeResolution: Envoy's start_time in the access log has millisecond
// precision (truncated).
const startTimeResolution = time.Millisecond

// readCalibIngress parses the access log written by collect-timeline.sh
// ("<collected> {json}" per line). It returns nil, nil when the file does
// not exist.
func readCalibIngress(path string) (*calibIngress, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ing := &calibIngress{start: map[string]time.Time{}, lastByIP: map[string]time.Time{}}
	// unusable: attempt ids seen on an ambiguous line or more than once; they
	// stay out of start however many more lines follow.
	unusable := map[string]bool{}
	seen := map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		i := bytes.IndexByte(line, '{')
		if i < 0 {
			continue
		}
		var l struct {
			Attempt          *string         `json:"attempt"`
			StartTime        string          `json:"start_time"`
			UpstreamHost     string          `json:"upstream_host"`
			UpstreamAttempts json.RawMessage `json:"upstream_attempts"`
		}
		if json.Unmarshal(line[i:], &l) != nil {
			continue
		}
		st, err := time.Parse(time.RFC3339Nano, l.StartTime)
		if err != nil {
			continue
		}
		host, _, herr := net.SplitHostPort(l.UpstreamHost)
		var n int
		single := json.Unmarshal(l.UpstreamAttempts, &n) == nil && n == 1
		if herr == nil && !single {
			// Retried inside the ingress (or not recorded): the final
			// upstream does not say which Pod was selected at start_time.
			ing.ambiguous++
		}
		if herr == nil && single && st.After(ing.lastByIP[host]) {
			ing.lastByIP[host] = st
		}
		if l.Attempt == nil || *l.Attempt == "" || *l.Attempt == "-" {
			continue
		}
		a := *l.Attempt
		seen[a]++
		if seen[a] == 2 {
			ing.dups++
		}
		if !single || seen[a] > 1 {
			unusable[a] = true
		}
		if unusable[a] {
			delete(ing.start, a)
			continue
		}
		ing.start[a] = st
	}
	return ing, sc.Err()
}

// join sets IngressSelectedAt on every answered probe that has a line and
// returns how many answered probes have none.
func (ing *calibIngress) join(probes []probeRec) (missing int) {
	for i := range probes {
		p := &probes[i]
		if p.PodUID == "" {
			continue
		}
		st, ok := ing.start[p.Attempt]
		if !ok {
			missing++
			continue
		}
		// The selection happened at or after both SentAt and the truncated
		// start_time; inside the truncation, SentAt is the tighter bound.
		if st.Before(p.SentAt) && p.SentAt.Sub(st) < startTimeResolution {
			st = p.SentAt
		}
		sel := st
		p.IngressSelectedAt = &sel
	}
	return missing
}
