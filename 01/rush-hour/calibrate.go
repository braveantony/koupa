package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
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
	Valid          bool     `json:"valid"`
	InvalidReason  string   `json:"invalid_reason"`
}

type calibReport struct {
	Model       string        `json:"model"`
	Config      calib.Config  `json:"config"`
	PreStopS    float64       `json:"prestop_s"`
	Upper       string        `json:"upper_bound_method"`
	DirectNote  string        `json:"direct_upper_note"`
	FreshConn   bool          `json:"fresh_connections"`
	PerAttempt  bool          `json:"per_attempt_ingress"`
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
	}
	var all []calib.Sample
	for _, dir := range dirs {
		probes, noUID, err := readProbes(filepath.Join(dir, "probes.jsonl"))
		if err != nil {
			return rep, err
		}
		rep.ProbesNoUID += noUID
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
			in := calib.Input{Probes: probes, TargetPodUID: m.PodUID, DeletionStart: del,
				PreStopEnd: del.Add(preStop), DirectUpper: nil,
				PerAttemptIngress: rep.PerAttempt, FreshConnections: rep.FreshConn}
			s := calib.Estimate(in, cfg)
			all = append(all, s)
			rep.Samples = append(rep.Samples, toCalibSample(filepath.Base(dir), m.PodUID, m.PodName, in, s))
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
func readProbes(path string) (probes []calib.Probe, noUID int, err error) {
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
		probes = append(probes, calib.Probe{SentAt: a.SentAt, RespondedAt: a.RespondedAt, PodUID: a.PodUID})
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
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "run\tpod\tdeletion\tL\tU\tkind\tp\tk\tn\tU-del(s)\tvalid/reason")
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%.3f\t%d\t%d\t%s\t%s\n", s.Run, s.PodName, short(&del),
			short(s.Lower), short(s.Upper), s.UpperKind, s.P, s.K, s.N, ud, v)
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
