package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"koupa/internal/wire"
	"koupa/rush-hour/verdict"
	"koupa/rush-hour/winddown"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// ------------------------------------------------------------ 7. states

// cmdReached: a request paused at request.mid on Pod P, P deleted with
// GRACEFUL_SHUTDOWN=0; the client sees the connection end. The client
// connects to the Pod IP directly (--pod-addr), no ingress.
func cmdReached(args []string) (any, error) {
	var logs, ns, addrs string
	_, _, run, hk, err := common("reached", args, func(fs *flag.FlagSet) {
		fs.StringVar(&logs, "logs", "", "followed log directory")
		fs.StringVar(&ns, "ns", "koupa", "namespace")
		fs.StringVar(&addrs, "pod-addr", "", "POD=IP:8080 of the Pod (direct, no ingress)")
	})
	if err != nil {
		return nil, err
	}
	pod, addr, ok := strings.Cut(addrs, "=")
	if !ok {
		return nil, errors.New("--pod-addr POD=HOST:PORT required")
	}
	h := parseHooks(*hk)
	out := map[string]any{"pod": pod}
	defer h.release("request.mid")
	if err := h.arm("request.mid", false); err != nil {
		return out, err
	}
	att := *run + "-a1-t1"
	step("send order directly to %s (attempt %s)", pod, att)
	cl := goDo(90*time.Second, http.MethodPost, "http://"+addr+"/orders", att, order(*run+"-c1"))
	rep, err := h.wait("request.mid", 20*time.Second)
	out["report"] = rep
	if err != nil {
		cl.cancel()
		return out, err
	}
	if rep.PodName != pod {
		cl.cancel()
		return out, fmt.Errorf("paused on %s, want %s", rep.PodName, pod)
	}
	if out["deleted_at"], err = deletePod(ns, pod); err != nil {
		cl.cancel()
		return out, err
	}
	r, err := cl.result(60 * time.Second)
	out["client"] = r
	if err != nil {
		return out, err
	}
	sig, err := waitLog(logs, pod, "sigterm", "", 30*time.Second)
	out["sigterm_at"] = sig
	return out, err
}

// cmdFlushOK: a raw HTTP client sends POST /orders directly to a Pod and
// never reads; the request pauses at order.after_commit, is released, the
// App writes and flushes the response (into the local network stack) and
// logs request_end; then the client gives up and closes without having
// read anything: no full response for the client.
func cmdFlushOK(args []string) (any, error) {
	var logs, addrs string
	_, _, run, hk, err := common("flushok", args, func(fs *flag.FlagSet) {
		fs.StringVar(&logs, "logs", "", "followed log directory")
		fs.StringVar(&addrs, "pod-addr", "", "POD=IP:8080 of the Pod (direct, no ingress)")
	})
	if err != nil {
		return nil, err
	}
	pod, addr, ok := strings.Cut(addrs, "=")
	if !ok {
		return nil, errors.New("--pod-addr POD=HOST:PORT required")
	}
	h := parseHooks(*hk)
	out := map[string]any{"pod": pod}
	defer h.release("order.after_commit")
	if err := h.arm("order.after_commit", false); err != nil {
		return out, err
	}
	att := *run + "-a1-t1"
	body, _ := json.Marshal(order(*run + "-c1"))
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return out, err
	}
	defer conn.Close()
	res := Result{Attempt: att, SentAt: now()}
	step("raw client sends order to %s (attempt %s) and never reads", pod, att)
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "POST /orders HTTP/1.1\r\nHost: koupa\r\nContent-Type: application/json\r\n%s: %s\r\nContent-Length: %d\r\n\r\n%s",
		wire.HeaderAttempt, att, len(body), body); err != nil {
		return out, err
	}
	rep, err := h.wait("order.after_commit", 20*time.Second)
	out["report"] = rep
	if err != nil {
		return out, err
	}
	if err := h.release("order.after_commit"); err != nil {
		return out, err
	}
	end, err := waitLog(logs, pod, "request_end", att, 20*time.Second)
	out["request_end_at"] = end
	if err != nil {
		return out, err
	}
	time.Sleep(time.Second)
	step("client gives up and closes without reading")
	conn.Close()
	res.RespondedAt = now()
	res.Err = "client closed without reading the response"
	out["client"] = res
	return out, nil
}

// cmdClassify runs verdict.State on attempts read from the collector's
// App logs (with the collector's meta, so coverage is the A11 one). The
// attempts file is JSON lines of Result; the client connected to the Pod
// IP directly (no ingress), so the upstream count is exactly 1.
func cmdClassify(args []string) (any, error) {
	fs := flag.NewFlagSet("classify", flag.ContinueOnError)
	dir := fs.String("app-logs", "", "collector app/ directory")
	attempts := fs.String("attempts", "", "JSON lines of client results")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(*attempts)
	if err != nil {
		return nil, err
	}
	app := winddown.ReadAppLogs(*dir)
	if !app.Available {
		return nil, fmt.Errorf("app logs: %s", app.Err)
	}
	type row struct {
		Attempt     string `json:"attempt"`
		Pod         string `json:"pod_uid"`
		LogCovered  bool   `json:"log_covered"`
		Received    bool   `json:"app_received"`
		Ended       bool   `json:"app_ended"`
		State       string `json:"state"`
		PodCoverage string `json:"pod_coverage"`
	}
	var rows []row
	for _, l := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		var r Result
		if err := json.Unmarshal(l, &r); err != nil {
			return nil, err
		}
		sent, _ := time.Parse(time.RFC3339Nano, r.SentAt)
		resp, _ := time.Parse(time.RFC3339Nano, r.RespondedAt)
		one := 1
		e := verdict.AttemptEvidence{
			FullSuccess: r.FullSuccess,
			Ingress:     &verdict.IngressRecord{Finished: true, UpstreamAttempts: &one},
		}
		recv, ends := app.Received[r.Attempt], app.Ends[r.Attempt]
		pod := ""
		if len(recv) > 0 {
			e.AppReceived = true
			t := recv[0].At
			e.AppReceivedAt = &t
			pod = recv[0].Pod
		}
		if len(ends) > 0 {
			end := ends[0].End
			e.AppEnd = &end
			if pod == "" {
				pod = ends[0].Pod
			}
		}
		e.Contradictory = app.Contradictory[r.Attempt]
		cov := "no such Pod"
		if p := app.Pods[pod]; p != nil {
			e.LogCovered = p.CoveredAt(sent) && p.CoveredAt(resp)
			cov = fmt.Sprintf("covered=%v meta=%v", p.Covered, p.Meta != nil)
		}
		rows = append(rows, row{r.Attempt, pod, e.LogCovered, e.AppReceived, e.AppEnd != nil,
			string(verdict.State(e, r.FullResponse && !r.FullSuccess)), cov})
	}
	return rows, nil
}
