package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"koupa/rush-hour/sse"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// ------------------------------------------------------------- 1. retry

// cmdRetry is A12 timing 1: arm order.after_commit (hold=1, so the pause
// survives the client closing), send, wait for the report, the client
// closes, release, resend the same client_ref, count the orders for it.
// The script checks from the App log that hook_released came after the
// close.
func cmdRetry(args []string) (any, error) {
	_, base, run, hk, err := common("retry", args, nil)
	if err != nil {
		return nil, err
	}
	h := parseHooks(*hk)
	ref := *run + "-c1"
	out := map[string]any{"client_ref": ref}
	defer h.release("order.after_commit")
	if err := h.arm("order.after_commit", true); err != nil {
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
	out["closed_at"] = now()
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
