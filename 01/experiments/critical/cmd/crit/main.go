// Command crit is the client side of the critical-timing experiments (plan
// A12, A10 must-tests that use pause points). The shell scripts in
// experiments/critical set up the cluster (overlay, port-forwards, log
// followers) and check the expected results; crit performs each timing's
// ordered steps, every step with its own timeout, and prints one JSON
// result on stdout.
//
//	crit arm|release NAME --hooks H [--hold]
//	crit wait NAME --hooks H [--timeout D]
//	crit retry      --base URL --run R --hooks H
//	crit sigterm    --base URL --run R --hooks H --graceful 0|1 --logs DIR
//	crit handoff    --base URL --run R --hooks H
//	crit gap        --base URL --run R
//	crit reached    --pod-addr POD=IP:8080 --run R --hooks H --logs DIR (07)
//	crit flushok    --pod-addr POD=IP:8080 --run R --hooks H --logs DIR (07)
//	crit classify   --app-logs DIR --attempts FILE            (07: verdict.State)
//	crit rescore    --dir RUN                                 (06: scorecard again)
//
// H is a comma-separated list of hook control addresses (one per koupa Pod,
// via kubectl port-forward). DIR holds one `kubectl logs -f` file per Pod
// named <pod-name>.log.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: crit arm|release|wait|retry|sigterm|handoff|gap|reached|flushok|classify|rescore ...")
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var (
		out any
		err error
	)
	switch cmd {
	case "arm", "release", "wait":
		out, err = cmdHook(cmd, args)
	case "retry":
		out, err = cmdRetry(args)
	case "sigterm":
		out, err = cmdSigterm(args)
	case "handoff":
		out, err = cmdHandoff(args)
	case "gap":
		out, err = cmdGap(args)
	case "reached":
		out, err = cmdReached(args)
	case "flushok":
		out, err = cmdFlushOK(args)
	case "classify":
		out, err = cmdClassify(args)
	case "rescore":
		out, err = cmdRescore(args)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if out != nil {
		_ = json.NewEncoder(os.Stdout).Encode(out)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "crit "+cmd+":", err)
		os.Exit(1)
	}
}

// step logs one step to stderr, so the evidence shows the order.
func step(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().UTC().Format("15:04:05.000"), fmt.Sprintf(format, a...))
}
