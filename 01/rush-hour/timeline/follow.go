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
	Ended                string `json:"ended"` // container_exited | pod_gone | stopped | ""
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

	out   io.Writer
	st    FollowState
	tail  []string // received lines of the last second
	total int
}

func (f *Follower) statePath() string { return filepath.Join(f.Dir, f.UID+".collect.json") }

func (f *Follower) save() {
	b, _ := json.MarshalIndent(f.st, "", "  ")
	tmp := f.statePath() + ".tmp"
	if os.WriteFile(tmp, append(b, '\n'), 0o644) == nil {
		_ = os.Rename(tmp, f.statePath())
	}
}

func (f *Follower) emit(l string, fromStart bool) {
	fmt.Fprintf(f.out, "%s %s\n", FormatStamp(f.Now()), l)
	f.total++
	if fromStart && strings.Contains(l, `"event":"startup"`) {
		f.st.StartupFromBeginning = true
	}
	ts := lineTS(l)
	if len(f.tail) > 0 && ts.Truncate(time.Second).After(lineTS(f.tail[len(f.tail)-1]).Truncate(time.Second)) {
		f.tail = f.tail[:0]
	}
	f.tail = append(f.tail, l)
}

func (f *Follower) kubectl(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, f.Kubectl, append([]string{"-n", f.Namespace}, args...)...)
}

// read runs one kubectl logs; follow=false for the final drain check. With
// lines already received it is a refill (--since-time): gap, when non-nil,
// gets Verified; a drain check (gap nil) that finds unseen or mismatched
// lines records its own gap. settled=false when a refill returned nothing
// and failed, so the overlap could not be checked yet.
func (f *Follower) read(ctx context.Context, follow bool, gap *Gap) (werr error, stderrS string, settled bool) {
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
	settle := func() {
		newLines, ok := MergeRefill(f.tail, buf)
		if gap != nil {
			gap.Verified = ok
		} else if !ok || len(newLines) > 0 {
			// Drain check found lines we never got, or could not match.
			f.st.Gaps = append(f.st.Gaps, Gap{After: last, Reason: fmt.Sprintf("串流結束後補讀，和已收到的行對不上（最後一秒收到 %d 行，補讀 %d 行，新行 %d）", len(f.tail), len(buf), len(newLines)), Refilled: true, Verified: ok})
		}
		pending = false
		for _, l := range newLines {
			f.emit(l, false)
		}
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
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
		f.emit(l, !refill)
	}
	werr = cmd.Wait()
	if junk != "" && werr == nil {
		werr = fmt.Errorf("kubectl logs returned a non-log line: %.200q", junk)
	}
	// An empty answer proves nothing (the container log may already be
	// gone): leave it unsettled; a follow refill that ended cleanly with
	// nothing new is settled (the overlap is then simply missing).
	if pending && len(buf) == 0 && (gap == nil || werr != nil) {
		return werr, strings.TrimSpace(stderr.String()), false
	}
	if pending && ctx.Err() == nil {
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
	var gap *Gap // an interruption waiting for its refill
	unfilled := func(why string) {
		if gap != nil {
			f.st.Gaps = append(f.st.Gaps, Gap{After: gap.After, Reason: gap.Reason + "；" + why})
			gap = nil
		}
	}
	for {
		werr, stderr, settled := f.read(ctx, true, gap)
		if gap != nil && settled {
			f.st.Gaps = append(f.st.Gaps, *gap)
			gap = nil
		}
		f.st.Segments++
		if ctx.Err() != nil {
			unfilled("收集已停止，沒有補讀")
			return stop("stopped")
		}
		reason := "kubectl logs 結束"
		if werr != nil {
			reason = fmt.Sprintf("kubectl logs: %v: %s", werr, stderr)
			f.st.LastError = reason
		}
		f.save()
		if f.total == 0 {
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
			// Drain check: one more read must add nothing unverified (and
			// fills a pending gap).
			nGaps := len(f.st.Gaps)
			derr, dstderr, dsettled := f.read(ctx, false, gap)
			switch {
			case !dsettled:
				f.st.DrainCheck = fmt.Sprintf("補讀沒有回傳任何行（%v %s）", derr, dstderr)
			case len(f.st.Gaps) > nGaps:
				f.st.DrainCheck = "補讀和已收到的行對不上"
			default:
				f.st.DrainCheck = "補讀逐行核對相符，沒有漏收"
			}
			if gap != nil && dsettled {
				f.st.Gaps = append(f.st.Gaps, *gap)
				gap = nil
			}
			unfilled("補讀失敗：" + dstderr)
			if !dsettled && werr != nil {
				// The stream ended with an error and the drain read could not
				// check it. (A clean EOF at container exit needs no check.)
				f.st.Gaps = append(f.st.Gaps, Gap{After: lineTS(f.tail[len(f.tail)-1]), Reason: reason + "；補讀失敗：" + dstderr})
			}
			return stop("container_exited")
		case "gone":
			unfilled("Pod 已不存在，無法補讀")
			if werr != nil {
				f.st.Gaps = append(f.st.Gaps, Gap{After: lineTS(f.tail[len(f.tail)-1]), Reason: reason + "；Pod 已不存在，無法補讀"})
			}
			return stop("pod_gone")
		default:
			// A break while the container runs (or state unknown): refill
			// with --since-time on the next round.
			if gap == nil {
				gap = &Gap{After: lineTS(f.tail[len(f.tail)-1]), Reason: reason, Refilled: true}
			}
			if !sleep(ctx, f.Poll) {
				unfilled("收集已停止，沒有補讀")
				return stop("stopped")
			}
		}
	}
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
