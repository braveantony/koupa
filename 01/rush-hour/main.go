// Command rush-hour is the Koupa load and measurement tool.
//
//	rush-hour run --base URL --run ID [--duration 60s] [--rate 20] [--boards 3] --out DIR
//	              [--ingress-log FILE] [--app-logs DIR] [--tx-timeout 60s] [--verbose] [--no-ingress]
//	rush-hour probe --base URL --run ID --duration 30s --out DIR [--rate 50]
//	rush-hour summarize DIR...
//	rush-hour calibrate [--prestop 20s] [--json FILE] DIR...  (preStop calibration samples, A11)
//	rush-hour timeline DIR          (DIR/timeline.jsonl + timeline.txt)
//	rush-hour coverage DIR          (DIR/app/<uid>.meta.json, used by collect-timeline.sh stop)
//	rush-hour stamp [--json]        (stdin -> stdout, collector stamp per line)
//	rush-hour follow-log --pod NAME --uid UID --dir DIR [--namespace koupa]
//
// Exit status: 0 normal; 2 the measurement is invalid (or could not run).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"koupa/rush-hour/timeline"
	"koupa/rush-hour/winddown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage:
  rush-hour run --base URL --run ID --out DIR [flags]
  rush-hour probe --base URL --run ID --duration D --out DIR [--rate 50]
  rush-hour summarize DIR...
  rush-hour calibrate [--prestop 20s] [--json FILE] DIR...
  rush-hour timeline DIR
  rush-hour coverage DIR
  rush-hour stamp [--json]
  rush-hour follow-log --pod NAME --uid UID --dir DIR [--namespace koupa]`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "run":
		return cmdRun(ctx, args[1:], stdout, stderr)
	case "probe":
		return cmdProbe(ctx, args[1:], stderr)
	case "summarize":
		if len(args) < 2 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		out, err := winddown.Summarize(args[1:])
		if err != nil {
			fmt.Fprintln(stderr, "rush-hour summarize:", err)
			return 2
		}
		fmt.Fprint(stdout, out)
		return 0
	case "calibrate":
		return cmdCalibrate(args[1:], stdout, stderr)
	case "timeline":
		if len(args) != 2 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		evs, err := timeline.Build(args[1])
		if err == nil {
			err = timeline.Write(args[1], evs)
		}
		if err != nil {
			fmt.Fprintln(stderr, "rush-hour timeline:", err)
			return 2
		}
		return 0
	case "coverage":
		if len(args) != 2 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		if err := timeline.Finalize(args[1]); err != nil {
			fmt.Fprintln(stderr, "rush-hour coverage:", err)
			return 2
		}
		return 0
	case "stamp":
		fs := flag.NewFlagSet("stamp", flag.ContinueOnError)
		fs.SetOutput(stderr)
		js := fs.Bool("json", false, "input is a stream of JSON values; write each compacted on one line")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		now := func() time.Time { return time.Now().UTC() }
		if err := timeline.Stamp(os.Stdin, stdout, *js, now); err != nil {
			fmt.Fprintln(stderr, "rush-hour stamp:", err)
			return 1
		}
		return 0
	case "follow-log":
		return cmdFollowLog(ctx, args[1:], stderr)
	}
	fmt.Fprintln(stderr, usage)
	return 2
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c winddown.Config
	fs.StringVar(&c.Base, "base", "", "base URL of the shop (ingress), e.g. http://koupa.local")
	fs.StringVar(&c.Run, "run", "", "run id; client_refs are <run>-c<n>")
	fs.DurationVar(&c.Duration, "duration", 60*time.Second, "sending period D (at least 30s)")
	fs.Float64Var(&c.Rate, "rate", 20, "orders per second")
	fs.IntVar(&c.Boards, "boards", 3, "number of boards")
	fs.StringVar(&c.Out, "out", "", "output directory (must not already hold a run)")
	fs.StringVar(&c.IngressLog, "ingress-log", "", "ingress access log (JSON lines), written by measure.sh")
	fs.StringVar(&c.AppLogs, "app-logs", "", "directory of <pod-uid>.log (+ .meta.json), written by measure.sh")
	fs.BoolVar(&c.Verbose, "verbose", false, "include diagnostics in scorecard.txt")
	fs.DurationVar(&c.TxTimeout, "tx-timeout", 60*time.Second, "database transaction_timeout")
	fs.BoolVar(&c.NoIngress, "no-ingress", false, "LOCAL/TEST ONLY: no ingress in the path (direct to the App); count every attempt as one upstream attempt")
	fs.DurationVar(&c.Timeout, "timeout", 5*time.Second, "per-attempt client timeout")
	fs.IntVar(&c.Retries, "retries", 1, "retries after a failed attempt")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var bad []string
	if c.Base == "" || c.Run == "" || c.Out == "" {
		bad = append(bad, "--base, --run and --out are required")
	}
	if strings.ContainsAny(c.Run, `%_\`) {
		bad = append(bad, "--run must not contain %, _ or \\")
	}
	if c.Duration < 30*time.Second {
		bad = append(bad, "--duration must be at least 30s")
	}
	if c.Rate <= 0 || c.Boards < 1 || c.TxTimeout <= 0 || c.Retries < 0 {
		bad = append(bad, "--rate, --boards and --tx-timeout must be positive, --retries not negative")
	}
	if c.NoIngress && c.IngressLog != "" {
		bad = append(bad, "--no-ingress and --ingress-log exclude each other")
	}
	if len(bad) > 0 {
		fmt.Fprintln(stderr, "rush-hour run:", strings.Join(bad, "; "))
		return 2
	}
	c.Base = strings.TrimRight(c.Base, "/")
	c.Stdout = stdout
	sc, err := winddown.Run(ctx, c)
	if err != nil {
		fmt.Fprintln(stderr, "本次量測無效：", err)
		return 2
	}
	return sc.ExitCode()
}

func cmdProbe(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c winddown.ProbeConfig
	fs.StringVar(&c.Base, "base", "", "base URL")
	fs.StringVar(&c.Run, "run", "", "run id")
	fs.DurationVar(&c.Duration, "duration", 0, "how long to probe")
	fs.StringVar(&c.Out, "out", "", "output directory (probes.jsonl)")
	fs.Float64Var(&c.Rate, "rate", 50, "probes per second")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if c.Base == "" || c.Run == "" || c.Out == "" || c.Duration <= 0 || c.Rate <= 0 {
		fmt.Fprintln(stderr, "rush-hour probe: --base, --run, --out and a positive --duration are required")
		return 2
	}
	c.Base = strings.TrimRight(c.Base, "/")
	if err := winddown.Probe(ctx, c); err != nil {
		fmt.Fprintln(stderr, "rush-hour probe: 本次量測無效：", err)
		return 2
	}
	return 0
}

func cmdFollowLog(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("follow-log", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f timeline.Follower
	fs.StringVar(&f.Namespace, "namespace", "koupa", "namespace")
	fs.StringVar(&f.Pod, "pod", "", "Pod name")
	fs.StringVar(&f.UID, "uid", "", "Pod UID (file name)")
	fs.StringVar(&f.Dir, "dir", "", "app log directory")
	fs.StringVar(&f.Kubectl, "kubectl", "kubectl", "kubectl binary")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if f.Pod == "" || f.UID == "" || f.Dir == "" {
		fmt.Fprintln(stderr, "rush-hour follow-log: --pod, --uid and --dir are required")
		return 2
	}
	if err := f.Run(ctx); err != nil {
		fmt.Fprintln(stderr, "rush-hour follow-log:", err)
		return 1
	}
	return 0
}
