package timeline

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// FollowState is <dir>/app/<uid>.collect.json, kept up to date by the log
// follower and read by Finalize.
type FollowState struct {
	PodUID       string     `json:"pod_uid"`
	PodName      string     `json:"pod_name"`
	CollectStart time.Time  `json:"collect_start"`
	CollectEnd   *time.Time `json:"collect_end"`
	// StartupFromBeginning: the startup line came from a read that began
	// at the start of the container log.
	StartupFromBeginning bool   `json:"startup_from_beginning"`
	Segments             int    `json:"segments"` // kubectl logs invocations that returned lines or ended
	Gaps                 []Gap  `json:"gaps"`
	Ended                string `json:"ended"` // container_exited | pod_gone | stopped | error | ""
	LastError            string `json:"last_error,omitempty"`
	DrainCheck           string `json:"drain_check,omitempty"` // result of the read after the container exited
}

// lineTS parses the kubelet timestamp kubectl --timestamps puts first.
func lineTS(l string) time.Time {
	sp := strings.IndexByte(l, ' ')
	if sp < 0 {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, l[:sp])
	return t
}

// MergeRefill checks a re-read made with --since-time = (last line's ts,
// truncated to the second; kubelet includes that second). have is what was
// received so far (at least every line of that last second). Every refill
// line up to the last received ts must equal, in order, the received lines
// from that second on; then the gap is proven filled. newLines are the refill
// lines after the last received ts.
func MergeRefill(have, refill []string) (newLines []string, verified bool) {
	if len(have) == 0 {
		return refill, true
	}
	last := lineTS(have[len(have)-1])
	since := last.Truncate(time.Second)
	var h []string
	for _, l := range have {
		if !lineTS(l).Before(since) {
			h = append(h, l)
		}
	}
	var r0 []string
	for i, l := range refill {
		if lineTS(l).After(last) {
			newLines = refill[i:]
			break
		}
		r0 = append(r0, l)
	}
	if len(r0) != len(h) {
		return newLines, false
	}
	for i := range h {
		if h[i] != r0[i] {
			return newLines, false
		}
	}
	return newLines, true
}

// Follower collects one Pod's log: `kubectl logs -f --timestamps` from the
// beginning; after a break while the container runs it re-reads with
// --since-time and verifies the overlap; after the container exits it does
// one more non-follow read to prove the stream was drained.
type Follower struct {
	Kubectl   string // default "kubectl"
	Namespace string
	Pod, UID  string
	Container string // default koupa
	Dir       string // the app directory
	Poll      time.Duration
	Now       func() time.Time
	MaxLine   int // longest accepted line (default 16 MiB)

	afterRead func() // test hook: runs after every follow read
	afterWait func() // test hook: runs right after kubectl was reaped

	out     io.Writer
	st      FollowState
	fatal   error
	killed  bool     // the last read ended because ctx was cancelled
	pending int      // index in st.Gaps of the interruption awaiting its refill, or -1
	tail    []string // received lines of the last second
	total   int
}

func (f *Follower) statePath() string { return filepath.Join(f.Dir, f.UID+".collect.json") }

func (f *Follower) save() {
	b, _ := json.MarshalIndent(f.st, "", "  ")
	tmp := f.statePath() + ".tmp"
	if os.WriteFile(tmp, append(b, '\n'), 0o644) == nil {
		_ = os.Rename(tmp, f.statePath())
	}
}

func (f *Follower) emit(l string, fromStart bool) error {
	if _, err := fmt.Fprintf(f.out, "%s %s\n", FormatStamp(f.Now()), l); err != nil {
		return err
	}
	f.total++
	if fromStart && strings.Contains(l, `"event":"startup"`) {
		f.st.StartupFromBeginning = true
	}
	ts := lineTS(l)
	if len(f.tail) > 0 && ts.Truncate(time.Second).After(lineTS(f.tail[len(f.tail)-1]).Truncate(time.Second)) {
		f.tail = f.tail[:0]
	}
	f.tail = append(f.tail, l)
	return nil
}

func (f *Follower) kubectl(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, f.Kubectl, append([]string{"-n", f.Namespace}, args...)...)
	cmd.SysProcAttr = childAttr()
	return cmd
}

// read runs one kubectl logs; follow=false for the final drain check. With
// lines already received it is a refill (--since-time) that must reproduce
// the overlap line for line. The result of the check goes to the pending
// gap (an interruption recorded earlier); a drain check without a pending
// gap that finds unseen or mismatched lines records its own gap. answered is
// false when the overlap could not be checked (no log lines came back).
// Scanner and log-write errors are returned as the stream's error.
func (f *Follower) read(ctx context.Context, follow bool) (werr error, stderrS string, answered bool) {
	args := []string{"logs", "--timestamps", "-c", f.Container, "pod/" + f.Pod}
	if follow {
		args = append(args, "-f")
	}
	refill := len(f.tail) > 0
	var last time.Time
	if refill {
		last = lineTS(f.tail[len(f.tail)-1])
		args = append(args, "--since-time="+last.Truncate(time.Second).Format(time.RFC3339))
	}
	cmd := f.kubectl(ctx, args...)
	cmd.WaitDelay = 2 * time.Second
	// Our stop: the flag is set immediately before we signal kubectl, so a
	// signal exit counts as ours only if we had already sent it.
	var stopSent atomic.Bool
	cmd.Cancel = func() error {
		stopSent.Store(true)
		return cmd.Process.Kill()
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err, "", !refill
	}
	if err := cmd.Start(); err != nil {
		return err, "", !refill
	}
	pending := refill
	var buf []string
	var emitErr error
	emitAll := func(lines []string) {
		for _, l := range lines {
			if err := f.emit(l, !refill); err != nil && emitErr == nil {
				emitErr = err
			}
		}
	}
	settle := func() {
		newLines, ok := MergeRefill(f.tail, buf)
		if f.pending >= 0 {
			g := &f.st.Gaps[f.pending]
			g.Refilled, g.Verified = true, ok
			f.pending = -1
		} else if !ok || len(newLines) > 0 {
			f.st.Gaps = append(f.st.Gaps, Gap{After: last, Reason: fmt.Sprintf("串流結束後補讀，和已收到的行對不上（最後一秒收到 %d 行，補讀 %d 行，新行 %d）", len(f.tail), len(buf), len(newLines)), Refilled: true, Verified: ok})
		}
		pending = false
		f.save()
		emitAll(newLines)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, min(64<<10, f.maxLine())), f.maxLine()) // the limit is max(cap, max)
	var junk string
	for sc.Scan() {
		l := sc.Text()
		if lineTS(l).IsZero() {
			// Not a log line: with --timestamps every real line starts with
			// the kubelet time. The kubelet answers "unable to retrieve
			// container logs ..." this way once the log file is gone.
			junk = l
			continue
		}
		if pending {
			buf = append(buf, l)
			if lineTS(l).After(last) {
				settle()
			}
			continue
		}
		emitAll([]string{l})
	}
	scanErr := sc.Err()
	if scanErr != nil && cmd.Process != nil {
		_ = cmd.Process.Kill() // the rest of the stream is lost
	}
	werr = cmd.Wait()
	if f.afterWait != nil {
		f.afterWait()
	}
	// Ended by our own cancellation (killed by a signal after ctx was done),
	// as opposed to a stream that ended or failed by itself.
	f.killed = stopSent.Load() && scanErr == nil && emitErr == nil &&
		(cmd.ProcessState == nil || !cmd.ProcessState.Exited())
	switch {
	case scanErr != nil:
		werr = fmt.Errorf("reading kubectl output: %w", scanErr)
		f.fatal = werr
	case emitErr != nil:
		werr = fmt.Errorf("writing the log file: %w", emitErr)
		f.fatal = werr
	case junk != "" && werr == nil:
		werr = fmt.Errorf("kubectl logs returned a non-log line: %.200q", junk)
	}
	if pending && len(buf) == 0 {
		return werr, strings.TrimSpace(stderr.String()), false
	}
	if pending && ctx.Err() == nil && scanErr == nil {
		settle()
	}
	return werr, strings.TrimSpace(stderr.String()), !pending
}

// podState is "running", "terminated", "gone", "waiting" or "unknown".
func (f *Follower) podState(ctx context.Context) string {
	cmd := f.kubectl(ctx, "get", "pod", f.Pod, "-o", "json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if strings.Contains(stderr.String(), "NotFound") || strings.Contains(stderr.String(), "not found") {
			return "gone"
		}
		return "unknown"
	}
	var p podObj
	if json.Unmarshal(out, &p) != nil {
		return "unknown"
	}
	if p.Metadata.UID != f.UID {
		return "gone"
	}
	c := p.appContainer()
	switch {
	case c == nil:
		return "waiting"
	case c.State.Running != nil:
		return "running"
	case c.State.Terminated != nil:
		return "terminated"
	}
	return "waiting"
}

// Run follows until the container exits, the Pod disappears or ctx ends.
func (f *Follower) Run(ctx context.Context) error {
	if f.Kubectl == "" {
		f.Kubectl = "kubectl"
	}
	if f.Container == "" {
		f.Container = AppContainer
	}
	if f.Poll == 0 {
		f.Poll = 500 * time.Millisecond
	}
	if f.Now == nil {
		f.Now = func() time.Time { return time.Now().UTC() }
	}
	lf, err := os.OpenFile(filepath.Join(f.Dir, f.UID+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	f.out = lf
	f.st = FollowState{PodUID: f.UID, PodName: f.Pod, CollectStart: f.Now()}
	f.save()
	stop := func(reason string) error {
		now := f.Now()
		f.st.CollectEnd = &now
		f.st.Ended = reason
		f.save()
		return nil
	}
	f.pending = -1
	for {
		werr, stderr, _ := f.read(ctx, true)
		if f.afterRead != nil {
			f.afterRead()
		}
		f.st.Segments++
		reason := "kubectl logs 結束"
		if werr != nil {
			reason = fmt.Sprintf("kubectl logs: %v: %s", werr, stderr)
			f.st.LastError = reason
		}
		f.save()
		if f.fatal != nil {
			return f.fatalStop(stop)
		}
		if ctx.Err() != nil {
			if f.killed {
				// The stop ended a healthy stream (a pending interruption
				// stays unverified; it is already recorded).
				return stop("stopped")
			}
			// The stream ended by itself just as the stop came: an error is
			// an interruption that can no longer be refilled; a clean EOF
			// is fine only if the container had exited.
			if werr == nil {
				qctx, qcancel := context.WithTimeout(context.Background(), 5*time.Second)
				st := f.podState(qctx)
				qcancel()
				switch st {
				case "terminated":
					return stop("container_exited")
				case "gone":
					return stop("pod_gone")
				}
			}
			f.interrupt(reason + "（收集同時停止，沒有補讀）")
			return stop("stopped")
		}
		if f.total == 0 && f.pending < 0 {
			// Nothing read yet (container not started): retry from the start.
			switch f.podState(ctx) {
			case "gone":
				return stop("pod_gone")
			case "terminated":
				if werr == nil {
					return stop("container_exited")
				}
			}
			if !sleep(ctx, f.Poll) {
				return stop("stopped")
			}
			continue
		}
		switch f.podState(ctx) {
		case "terminated":
			if werr != nil {
				f.interrupt(reason)
			}
			// Drain check: one more read must add nothing unverified (and
			// fills a pending interruption).
			hadPending := f.pending >= 0
			nGaps := len(f.st.Gaps)
			derr, dstderr, answered := f.read(ctx, false)
			switch {
			case !answered:
				f.st.DrainCheck = fmt.Sprintf("補讀沒有回傳任何行（%v %s）", derr, dstderr)
			case len(f.st.Gaps) > nGaps || (hadPending && !f.st.Gaps[nGaps-1].Verified):
				f.st.DrainCheck = "補讀和已收到的行對不上"
			default:
				f.st.DrainCheck = "補讀逐行核對相符，沒有漏收"
			}
			if f.fatal != nil {
				f.st.DrainCheck = "補讀失敗：" + f.fatal.Error()
				return f.fatalStop(stop)
			}
			return stop("container_exited")
		case "gone":
			if werr != nil {
				f.interrupt(reason + "；Pod 已不存在，無法補讀")
			}
			return stop("pod_gone")
		default:
			// A break while the container runs (or state unknown): it is
			// recorded now and refilled with --since-time on the next round.
			f.interrupt(reason)
			if !sleep(ctx, f.Poll) {
				return stop("stopped")
			}
		}
	}
}

// fatalStop: lines were lost on our side (unreadable kubectl output or the
// log file could not be written); no refill can prove the log whole.
func (f *Follower) fatalStop(stop func(string) error) error {
	f.st.Gaps = append(f.st.Gaps, Gap{After: f.lastTS(), Reason: f.fatal.Error() + "（收集停止，log 不完整）"})
	f.pending = -1
	return stop("error")
}

func (f *Follower) lastTS() time.Time {
	if len(f.tail) == 0 {
		return time.Time{}
	}
	return lineTS(f.tail[len(f.tail)-1])
}

// interrupt records a stream interruption at once, unverified. A refill
// that reproduces the overlap marks it verified; until then (or if the
// collection stops first) it counts against coverage. A second break before
// the refill answered is covered by the same since-time, so it joins it.
func (f *Follower) interrupt(reason string) {
	if f.pending >= 0 {
		f.st.Gaps[f.pending].Reason += "；" + reason
	} else {
		f.st.Gaps = append(f.st.Gaps, Gap{After: f.lastTS(), Reason: reason})
		f.pending = len(f.st.Gaps) - 1
	}
	f.save()
}

func (f *Follower) maxLine() int {
	if f.MaxLine > 0 {
		return f.MaxLine
	}
	return 16 << 20
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Stamp copies r to w, prefixing every line with the collector stamp. With
// jsonStream the input is a stream of (possibly pretty-printed) JSON values,
// each written compacted on one line.
func Stamp(r io.Reader, w io.Writer, jsonStream bool, now func() time.Time) error {
	if jsonStream {
		dec := json.NewDecoder(r)
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
			var b bytes.Buffer
			if err := json.Compact(&b, raw); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "%s %s\n", FormatStamp(now()), b.Bytes()); err != nil {
				return err
			}
		}
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		if _, err := fmt.Fprintf(w, "%s %s\n", FormatStamp(now()), sc.Bytes()); err != nil {
			return err
		}
	}
	return sc.Err()
}
