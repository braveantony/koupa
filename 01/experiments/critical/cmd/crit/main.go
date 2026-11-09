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
//
// H is a comma-separated list of hook control addresses (one per koupa Pod,
// via kubectl port-forward). DIR holds one `kubectl logs -f` file per Pod
// named <pod-name>.log.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"koupa/internal/wire"
	"koupa/rush-hour/sse"
	"koupa/rush-hour/verdict"
	"koupa/rush-hour/winddown"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: crit arm|release|wait|retry|sigterm|handoff|gap|reached|flushok|classify ...")
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

// ------------------------------------------------------------------ hooks

type hooks []string

func parseHooks(s string) hooks {
	var h hooks
	for _, a := range strings.Split(s, ",") {
		if a = strings.TrimSpace(a); a != "" {
			h = append(h, a)
		}
	}
	return h
}

var ctl = &http.Client{Timeout: 10 * time.Second}

func (h hooks) post(name, action, query string) error {
	if len(h) == 0 {
		return errors.New("no hook addresses")
	}
	for _, a := range h {
		resp, err := ctl.Post("http://"+a+"/hooks/"+name+"/"+action+query, "", nil)
		if err != nil {
			return fmt.Errorf("%s %s on %s: %w", action, name, a, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("%s %s on %s: HTTP %d", action, name, a, resp.StatusCode)
		}
	}
	return nil
}

func (h hooks) arm(name string, hold bool) error {
	q := ""
	if hold {
		q = "?hold=1"
	}
	step("arm %s%s on %d Pod(s)", name, q, len(h))
	return h.post(name, "arm", q)
}

func (h hooks) release(name string) error {
	step("release %s on %d Pod(s)", name, len(h))
	return h.post(name, "release", "")
}

// Report is a wait reply: some request is paused at the point.
type Report struct {
	PodUID  string `json:"pod_uid"`
	PodName string `json:"pod_name"`
	Attempt string `json:"attempt"`
	At      string `json:"at"`
}

// wait polls every Pod's wait endpoint until one reports a pause.
func (h hooks) wait(name string, timeout time.Duration) (Report, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, a := range h {
			resp, err := ctl.Get("http://" + a + "/hooks/" + name + "/wait?timeout=300ms")
			if err != nil {
				continue // a Pod being deleted drops its port-forward
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				var r Report
				if err := json.Unmarshal(body, &r); err != nil {
					return r, err
				}
				r.At = now()
				step("pause reported: %s attempt %s", r.PodName, r.Attempt)
				return r, nil
			}
		}
	}
	return Report{}, fmt.Errorf("no pause at %s within %s", name, timeout)
}

func cmdHook(cmd string, args []string) (any, error) {
	if len(args) < 1 {
		return nil, errors.New("hook name required")
	}
	name := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	hk := fs.String("hooks", "", "hook addresses")
	hold := fs.Bool("hold", false, "arm with hold=1")
	timeout := fs.Duration("timeout", 30*time.Second, "wait timeout")
	if err := fs.Parse(args[1:]); err != nil {
		return nil, err
	}
	h := parseHooks(*hk)
	switch cmd {
	case "arm":
		return nil, h.arm(name, *hold)
	case "release":
		return nil, h.release(name)
	}
	r, err := h.wait(name, *timeout)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ------------------------------------------------------------------ HTTP

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Result is one client attempt as the client saw it.
type Result struct {
	Attempt      string          `json:"attempt"`
	SentAt       string          `json:"sent_at"`
	RespondedAt  string          `json:"responded_at"`
	Status       int             `json:"status,omitempty"`
	FullResponse bool            `json:"full_response"`
	FullSuccess  bool            `json:"full_success"`
	Err          string          `json:"err,omitempty"`
	PodUID       string          `json:"pod_uid,omitempty"`
	Body         json.RawMessage `json:"body,omitempty"`
}

// freshClient never reuses a connection, so closing one attempt never
// affects another.
func freshClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
}

func do(ctx context.Context, method, url, attempt string, body any, hdr map[string]string) Result {
	r := Result{Attempt: attempt}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if attempt != "" {
		req.Header.Set(wire.HeaderAttempt, attempt)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	r.SentAt = now()
	resp, err := freshClient().Do(req)
	if err != nil {
		r.RespondedAt, r.Err = now(), err.Error()
		return r
	}
	defer resp.Body.Close()
	r.Status = resp.StatusCode
	r.PodUID = resp.Header.Get(wire.HeaderPodUID)
	data, err := io.ReadAll(resp.Body)
	r.RespondedAt = now()
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.FullResponse = true
	r.FullSuccess = resp.StatusCode/100 == 2
	if json.Valid(data) {
		r.Body = data
	}
	return r
}

type orderBody struct {
	ClientRef string `json:"client_ref"`
	Items     []item `json:"items"`
}

type item struct {
	MenuItemID int `json:"menu_item_id"`
	Quantity   int `json:"quantity"`
}

func order(ref string) orderBody { return orderBody{ClientRef: ref, Items: []item{{1, 1}}} }

// async runs do in the background with its own cancel.
type async struct {
	cancel context.CancelFunc
	done   chan Result
}

func goDo(timeout time.Duration, method, url, attempt string, body any) *async {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	a := &async{cancel: cancel, done: make(chan Result, 1)}
	go func() { a.done <- do(ctx, method, url, attempt, body, nil) }()
	return a
}

func (a *async) result(timeout time.Duration) (Result, error) {
	select {
	case r := <-a.done:
		return r, nil
	case <-time.After(timeout):
		return Result{}, fmt.Errorf("client did not finish within %s", timeout)
	}
}

// placeOrder creates an order with no hook armed and returns its id.
func placeOrder(base, ref, attempt string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := do(ctx, http.MethodPost, base+"/orders", attempt, order(ref), nil)
	var o struct {
		OrderID int64 `json:"order_id"`
	}
	if !r.FullSuccess || json.Unmarshal(r.Body, &o) != nil || o.OrderID == 0 {
		return 0, fmt.Errorf("place order %s: %+v", ref, r)
	}
	return o.OrderID, nil
}

func markReady(base string, id int64, attempt string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := do(ctx, http.MethodPost, fmt.Sprintf("%s/orders/%d/ready", base, id), attempt, nil, nil)
	var o struct {
		ReadySeq int64 `json:"ready_seq"`
	}
	if !r.FullSuccess || json.Unmarshal(r.Body, &o) != nil || o.ReadySeq == 0 {
		return 0, fmt.Errorf("mark ready %d: %+v", id, r)
	}
	step("order %d ready_seq %d", id, o.ReadySeq)
	return o.ReadySeq, nil
}

func auditOrders(base, run string) (map[string][]int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := do(ctx, http.MethodGet, base+"/api/audit?mode=orders&run="+run, "", nil, nil)
	var m map[string][]int64
	if !r.FullSuccess || json.Unmarshal(r.Body, &m) != nil {
		return nil, fmt.Errorf("audit orders: %+v", r)
	}
	return m, nil
}

func common(name string, args []string, extra func(*flag.FlagSet)) (*flag.FlagSet, *string, *string, *string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	base := fs.String("base", "", "base URL")
	run := fs.String("run", "", "run id (client_ref prefix)")
	hk := fs.String("hooks", "", "hook addresses")
	if extra != nil {
		extra(fs)
	}
	err := fs.Parse(args)
	if err == nil && *run == "" {
		err = errors.New("--run required")
	}
	return fs, base, run, hk, err
}

// ------------------------------------------------------------- 1. retry

// cmdRetry is A12 timing 1: arm order.after_commit, send, wait for the
// report, the client closes, release, resend the same client_ref, count
// the orders for it.
func cmdRetry(args []string) (any, error) {
	_, base, run, hk, err := common("retry", args, nil)
	if err != nil {
		return nil, err
	}
	h := parseHooks(*hk)
	ref := *run + "-c1"
	out := map[string]any{"client_ref": ref}
	defer h.release("order.after_commit")
	if err := h.arm("order.after_commit", false); err != nil {
		return out, err
	}
	step("send order %s (attempt 1)", ref)
	first := goDo(30*time.Second, http.MethodPost, *base+"/orders", *run+"-a1-t1", order(ref))
	rep, err := h.wait("order.after_commit", 20*time.Second)
	out["report"] = rep
	if err != nil {
		first.cancel()
		return out, err
	}
	step("client closes the connection")
	first.cancel()
	r1, err := first.result(10 * time.Second)
	out["first"] = r1
	if err != nil {
		return out, err
	}
	if err := h.release("order.after_commit"); err != nil {
		return out, err
	}
	step("resend %s (attempt 2)", ref)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out["retry"] = do(ctx, http.MethodPost, *base+"/orders", *run+"-a1-t2", order(ref), nil)
	m, err := auditOrders(*base, *run)
	out["orders"] = m[ref]
	step("orders for %s: %v", ref, m[ref])
	return out, err
}

// ----------------------------------------------------------- 2. sigterm

// podState reads the Pod with kubectl: gone, or the koupa container's
// terminated finishedAt.
func podState(ns, pod string) (gone bool, finishedAt string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", "-n", ns, "get", "pod", pod, "-o", "json", "--ignore-not-found").Output()
	if err != nil {
		return false, ""
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return true, ""
	}
	var p struct {
		Status struct {
			ContainerStatuses []struct {
				Name  string `json:"name"`
				State struct {
					Terminated *struct {
						FinishedAt string `json:"finishedAt"`
					} `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if json.Unmarshal(out, &p) != nil {
		return false, ""
	}
	for _, c := range p.Status.ContainerStatuses {
		if c.Name == "koupa" && c.State.Terminated != nil {
			return false, c.State.Terminated.FinishedAt
		}
	}
	return false, ""
}

func deletePod(ns, pod string) (string, error) {
	at := now()
	step("kubectl delete pod %s --wait=false", pod)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", "-n", ns, "delete", "pod", pod, "--wait=false").CombinedOutput()
	if err != nil {
		return at, fmt.Errorf("delete %s: %v: %s", pod, err, out)
	}
	return at, nil
}

// waitLog waits until the Pod's followed log has a line with the event
// (and, if non-empty, the attempt); it returns the line's ts.
func waitLog(dir, pod, event, attempt string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		if ts := findLog(filepath.Join(dir, pod+".log"), event, attempt); ts != "" {
			step("log %s: %s %s at %s", pod, event, attempt, ts)
			return ts, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no %s %s in %s's log within %s", event, attempt, pod, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func findLog(path, event, attempt string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		l := sc.Bytes()
		i := bytes.IndexByte(l, '{')
		if i < 0 {
			continue
		}
		var m struct {
			TS      string `json:"ts"`
			Event   string `json:"event"`
			Attempt string `json:"attempt"`
		}
		if json.Unmarshal(l[i:], &m) == nil && m.Event == event && (attempt == "" || m.Attempt == attempt) {
			return m.TS
		}
	}
	return ""
}

// cmdSigterm is A12 timing 2.
func cmdSigterm(args []string) (any, error) {
	var graceful int
	var logs, ns string
	_, base, run, hk, err := common("sigterm", args, func(fs *flag.FlagSet) {
		fs.IntVar(&graceful, "graceful", 1, "GRACEFUL_SHUTDOWN of the overlay (0 or 1)")
		fs.StringVar(&logs, "logs", "", "followed log directory")
		fs.StringVar(&ns, "ns", "koupa", "namespace")
	})
	if err != nil {
		return nil, err
	}
	h := parseHooks(*hk)
	out := map[string]any{"graceful": graceful}
	defer h.release("request.mid")
	if err := h.arm("request.mid", false); err != nil {
		return out, err
	}
	att := *run + "-a1-t1"
	step("send order (attempt %s)", att)
	cl := goDo(90*time.Second, http.MethodPost, *base+"/orders", att, order(*run+"-c1"))
	rep, err := h.wait("request.mid", 20*time.Second)
	out["report"] = rep
	if err != nil {
		cl.cancel()
		return out, err
	}
	if out["deleted_at"], err = deletePod(ns, rep.PodName); err != nil {
		cl.cancel()
		return out, err
	}
	if graceful == 0 {
		// No wait for the SIGTERM line, no release: the program exits and
		// the paused request goes with it.
		deadline := time.Now().Add(90 * time.Second)
		for {
			gone, fin := podState(ns, rep.PodName)
			if gone || fin != "" {
				out["pod_gone"], out["finished_at"] = gone, fin
				out["ended_observed_at"] = now()
				step("Pod %s ended (gone=%v finishedAt=%q)", rep.PodName, gone, fin)
				break
			}
			if time.Now().After(deadline) {
				cl.cancel()
				return out, errors.New("Pod did not end within 90s")
			}
			time.Sleep(200 * time.Millisecond)
		}
		r, err := cl.result(60 * time.Second)
		out["client"] = r
		return out, err
	}
	sig, err := waitLog(logs, rep.PodName, "sigterm", "", 60*time.Second)
	out["sigterm_at"] = sig
	if err != nil {
		cl.cancel()
		return out, err
	}
	step("wait 2s (< SHUTDOWN_TIMEOUT)")
	time.Sleep(2 * time.Second)
	if err := h.release("request.mid"); err != nil {
		cl.cancel()
		return out, err
	}
	out["released_at"] = now()
	r, err := cl.result(20 * time.Second)
	out["client"] = r
	return out, err
}

// ------------------------------------------------------------- boards

type board struct {
	mu     sync.Mutex
	events []sse.Event
	cancel context.CancelFunc
	done   chan error
	status int
}

// openBoard connects GET /events (optionally with Last-Event-ID) and
// records every dispatched event until closed.
func openBoard(base, attempt, lastID string) *board {
	ctx, cancel := context.WithCancel(context.Background())
	b := &board{cancel: cancel, done: make(chan error, 1)}
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
		req.Header.Set(wire.HeaderAttempt, attempt)
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		resp, err := freshClient().Do(req)
		if err != nil {
			b.done <- err
			return
		}
		defer resp.Body.Close()
		b.mu.Lock()
		b.status = resp.StatusCode
		b.mu.Unlock()
		var p sse.Parser
		b.done <- p.Parse(resp.Body, func(e sse.Event) {
			b.mu.Lock()
			b.events = append(b.events, e)
			b.mu.Unlock()
		})
	}()
	return b
}

func (b *board) close() { b.cancel(); <-b.done }

func (b *board) snapshot() []sse.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]sse.Event(nil), b.events...)
}

// until waits for an event matching f.
func (b *board) until(f func(sse.Event) bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, e := range b.snapshot() {
			if f(e) {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

type evJSON struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

func (b *board) summary() (helloID string, ready []int64, all []evJSON) {
	for _, e := range b.snapshot() {
		all = append(all, evJSON{e.Type, e.ID})
		switch e.Type {
		case "hello":
			if helloID == "" {
				helloID = e.ID
			}
		case "ready":
			if n, err := strconv.ParseInt(e.ID, 10, 64); err == nil {
				ready = append(ready, n)
			}
		}
	}
	return
}

func isHello(e sse.Event) bool { return e.Type == "hello" }

// ----------------------------------------------------------- 3. handoff

// cmdHandoff is A12 timing 3: arm sse.after_hello, connect, wait, a new
// call commits, release, the board must get it exactly once.
func cmdHandoff(args []string) (any, error) {
	_, base, run, hk, err := common("handoff", args, nil)
	if err != nil {
		return nil, err
	}
	h := parseHooks(*hk)
	out := map[string]any{}
	id, err := placeOrder(*base, *run+"-c1", *run+"-a1-t1")
	if err != nil {
		return out, err
	}
	defer h.release("sse.after_hello")
	if err := h.arm("sse.after_hello", false); err != nil {
		return out, err
	}
	step("board connects")
	b := openBoard(*base, *run+"-b1", "")
	defer b.close()
	rep, err := h.wait("sse.after_hello", 20*time.Second)
	out["report"] = rep
	if err != nil {
		return out, err
	}
	seq, err := markReady(*base, id, *run+"-k1")
	out["ready_seq"] = seq
	if err != nil {
		return out, err
	}
	if err := h.release("sse.after_hello"); err != nil {
		return out, err
	}
	want := strconv.FormatInt(seq, 10)
	got := b.until(func(e sse.Event) bool { return e.Type == "ready" && e.ID == want }, 15*time.Second)
	step("board got %d: %v; listening 3s more for duplicates", seq, got)
	time.Sleep(3 * time.Second)
	out["hello_id"], out["ready"], out["events"] = b.summary()
	return out, nil
}

// --------------------------------------------------------------- 4. gap

// cmdGap is A12 timing 4: hello, close, a call commits, reconnect with
// Last-Event-ID.
func cmdGap(args []string) (any, error) {
	_, base, run, _, err := common("gap", args, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	id, err := placeOrder(*base, *run+"-c1", *run+"-a1-t1")
	if err != nil {
		return out, err
	}
	step("board connects")
	b := openBoard(*base, *run+"-b1", "")
	if !b.until(isHello, 15*time.Second) {
		b.close()
		return out, errors.New("no hello within 15s")
	}
	time.Sleep(500 * time.Millisecond)
	b.close()
	h1, r1, _ := b.summary()
	last := h1
	if len(r1) > 0 {
		last = strconv.FormatInt(r1[len(r1)-1], 10)
	}
	out["first_hello_id"], out["last_event_id"] = h1, last
	step("board closed; last event id %s", last)
	seq, err := markReady(*base, id, *run+"-k1")
	out["ready_seq"] = seq
	if err != nil {
		return out, err
	}
	step("board reconnects with Last-Event-ID %s", last)
	b2 := openBoard(*base, *run+"-b2", last)
	defer b2.close()
	if !b2.until(isHello, 15*time.Second) {
		return out, errors.New("no hello on reconnect within 15s")
	}
	// Longer than the broadcaster's poll interval (2s), so a live call
	// would have arrived too.
	time.Sleep(6 * time.Second)
	out["hello_id"], out["ready"], out["events"] = b2.summary()
	return out, nil
}

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
