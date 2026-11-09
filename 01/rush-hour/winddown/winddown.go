package winddown

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"koupa/rush-hour/load"
	"koupa/rush-hour/record"
	"koupa/rush-hour/sse"
	"koupa/rush-hour/verdict"
)

// Clock is the wind-down's time source; tests inject a fake one so the
// wait limits (up to transaction_timeout + 10s) run instantly.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RealClock is the wall clock.
var RealClock Clock = realClock{}

// Limits are the wind-down wait limits (A9).
type Limits struct {
	Client  time.Duration // step 2: client requests and retries
	Server  time.Duration // step 3/4: server-side settlement (transaction_timeout + 10s)
	Kitchen time.Duration // step 4: kitchen drain
	Boards  time.Duration // step 6: boards catch up to the cursor
	Hello   time.Duration // before sending: every board got hello
	Poll    time.Duration // re-check interval while waiting for settlement
}

// DefaultLimits are A9's limits for a database transaction_timeout.
func DefaultLimits(txTimeout time.Duration) Limits {
	return Limits{
		Client:  10 * time.Second,
		Server:  txTimeout + 10*time.Second,
		Kitchen: 10 * time.Second,
		Boards:  10 * time.Second,
		Hello:   10 * time.Second,
		Poll:    time.Second,
	}
}

// waitUntil calls pending until it returns 0 or limit has passed on clk,
// sleeping poll between calls; it returns the last pending count.
func waitUntil(clk Clock, limit, poll time.Duration, pending func(now time.Time) int) int {
	deadline := clk.Now().Add(limit)
	for {
		now := clk.Now()
		n := pending(now)
		if n == 0 || !now.Before(deadline) {
			return n
		}
		d := poll
		if rem := deadline.Sub(now); rem < d {
			d = rem
		}
		<-clk.After(d)
	}
}

// Config is `rush-hour run`.
type Config struct {
	Base       string
	Run        string
	Duration   time.Duration
	Rate       float64
	Boards     int
	Out        string
	IngressLog string
	AppLogs    string
	Verbose    bool
	TxTimeout  time.Duration
	NoIngress  bool
	Timeout    time.Duration // per attempt; 0 means 5s
	Retries    int
	Limits     Limits // zero means DefaultLimits(TxTimeout)
	Clock      Clock  // nil means RealClock
	Stdout     io.Writer
}

func (c *Config) paths() Paths {
	return Paths{
		Attempts: filepath.Join(c.Out, "attempts.jsonl"),
		Boards:   filepath.Join(c.Out, "boards.jsonl"),
		AppLogs:  c.AppLogs,
		Ingress:  c.IngressLog,
	}
}

// pendingSettle counts order/ready attempts not yet settled at now, reading
// the evidence afresh (files are still being appended to).
func pendingSettle(c *Config, now time.Time) int {
	p := c.paths()
	p.Boards = ""
	ev := LoadEvidence(p)
	n := 0
	for _, a := range ev.Attempts {
		if a.Kind != record.KindOrder && a.Kind != record.KindReady {
			continue
		}
		if !verdict.Settle(assemble(a, ev, c.NoIngress, nil), now, c.TxTimeout).Settled {
			n++
		}
	}
	return n
}

func newClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		MaxIdleConns: 256, MaxIdleConnsPerHost: 256, IdleConnTimeout: 90 * time.Second,
	}}
}

// Run is `rush-hour run`: send for Duration, wind down (A9 steps 1-6),
// reconcile (step 7) and write DIR/scorecard.{txt,json}. The returned error
// means the scorecard itself could not be written.
func Run(ctx context.Context, c Config) (Scorecard, error) {
	if c.Clock == nil {
		c.Clock = RealClock
	}
	if c.Limits == (Limits{}) {
		c.Limits = DefaultLimits(c.TxTimeout)
	}
	if c.Stdout == nil {
		c.Stdout = io.Discard
	}
	if c.Timeout == 0 {
		c.Timeout = 5 * time.Second
	}
	clk := c.Clock
	facts := Facts{
		Run: c.Run, TxTimeoutMs: c.TxTimeout.Milliseconds(), NoIngress: c.NoIngress, Retries: c.Retries,
		CaughtUp: map[string]bool{}, IngressPath: c.IngressLog, AppLogsDir: c.AppLogs,
	}
	var tlMu sync.Mutex
	mark := func(what string) {
		tlMu.Lock()
		facts.Timeline = append(facts.Timeline, TimelineEntry{clk.Now(), what})
		tlMu.Unlock()
	}
	invalid := func(format string, a ...any) { facts.Invalid = append(facts.Invalid, fmt.Sprintf(format, a...)) }

	if err := os.MkdirAll(c.Out, 0o755); err != nil {
		return Scorecard{}, err
	}
	p := c.paths()
	// O_EXCL: never mix two runs' evidence in one directory.
	af, err := os.OpenFile(p.Attempts, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return Scorecard{}, err
	}
	bf, err := os.OpenFile(p.Boards, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		af.Close()
		return Scorecard{}, err
	}
	aw, bw := record.NewWriter(af), record.NewBoardWriter(bf)
	client := newClient()
	defer client.CloseIdleConnections()
	wctx := context.WithoutCancel(ctx) // the wind-down itself always completes

	hctx, hcancel := context.WithCancel(wctx)
	hostDone := make(chan *HostStats, 1)
	go func() { hostDone <- sampleHost(hctx, time.Second) }()

	mark("開始連線看板")
	bctx, bcancel := context.WithCancel(wctx)
	var bwg sync.WaitGroup
	boards := make([]*sse.Board, c.Boards)
	for i := range boards {
		b := &sse.Board{Name: fmt.Sprintf("board-%d", i+1), RunID: c.Run, URL: c.Base + "/events", W: bw}
		boards[i] = b
		facts.CaughtUp[b.Name] = false
		bwg.Add(1)
		go func() { defer bwg.Done(); b.Run(bctx) }()
	}
	noHello := waitUntil(clk, c.Limits.Hello, 50*time.Millisecond, func(time.Time) int {
		n := 0
		for _, b := range boards {
			if _, ok := b.FirstStart(); !ok {
				n++
			}
		}
		return n
	})

	sent := false
	var cursor *int64
	if noHello > 0 {
		for _, b := range boards {
			if _, ok := b.FirstStart(); !ok {
				invalid("看板 %s 在 %s 內沒收到 hello（沒有起點），沒有開始送單", b.Name, c.Limits.Hello)
			}
		}
	} else {
		sent = true
		mark("看板都收到 hello")
		fmt.Fprintf(c.Stdout, "SENDING_STARTED %d\n", time.Now().UnixNano())
		mark("開始送單")
		cursor = c.sendAndWindDown(ctx, wctx, client, aw, &facts, mark, invalid)
	}

	// Step 6: boards catch up to C.
	if cursor != nil {
		C := *cursor
		waitUntil(clk, c.Limits.Boards, 100*time.Millisecond, func(time.Time) int {
			n := 0
			for _, b := range boards {
				if b.MaxSeq() < C {
					n++
				}
			}
			return n
		})
		behind := 0
		for _, b := range boards {
			facts.CaughtUp[b.Name] = b.MaxSeq() >= C
			if b.MaxSeq() < C {
				behind++
			}
		}
		if behind == 0 {
			mark("看板都追上截止游標")
		} else {
			mark(fmt.Sprintf("看板追上等待到上限，%d 台沒追上", behind))
		}
	}
	stop := clk.Now()
	facts.BoardsStoppedAt = &stop
	bcancel()
	bwg.Wait()
	mark("停止看板")
	hcancel()
	facts.Host = <-hostDone

	if err := aw.Err(); err != nil {
		invalid("attempts.jsonl 寫入失敗：%v", err)
	}
	if err := bw.Err(); err != nil {
		invalid("boards.jsonl 寫入失敗：%v", err)
	}
	if err := errors.Join(af.Close(), bf.Close()); err != nil {
		invalid("紀錄檔關閉失敗：%v", err)
	}
	if !sent {
		facts.Audit.Errors = append(facts.Audit.Errors, "沒有開始送單")
	}

	// Step 7.
	facts.EvaluatedAt = clk.Now()
	sc := Evaluate(facts, LoadEvidence(p))
	if err := WriteScorecard(c.Out, sc, c.Verbose); err != nil {
		return sc, err
	}
	fmt.Fprint(c.Stdout, sc.Text(c.Verbose))
	return sc, nil
}

// sendAndWindDown runs the sending period and A9 steps 1-5; it returns the
// cursor C (nil when the audit failed).
func (c *Config) sendAndWindDown(ctx, wctx context.Context, client *http.Client, aw *record.Writer,
	facts *Facts, mark func(string), invalid func(string, ...any)) *int64 {
	clk := c.Clock
	kitchen := &load.Kitchen{Client: client, Base: c.Base, RunID: c.Run, Timeout: c.Timeout, W: aw}
	kctx, kcancel := context.WithCancel(wctx)
	defer kcancel()
	kdone := make(chan error, 1)
	go func() { kdone <- kitchen.Run(kctx) }()

	customer := &load.Customer{Client: client, Base: c.Base, Run: c.Run, Timeout: c.Timeout,
		Retries: c.Retries, W: aw, Seed: time.Now().UnixNano()}
	pctx, pcancel := context.WithCancel(wctx)
	defer pcancel()
	var (
		mu       sync.Mutex
		inflight = map[string]bool{}
		placeErr error
		pwg      sync.WaitGroup
	)
	start := time.Now()
	end := start.Add(c.Duration)
	sched := &load.Scheduler{Rate: c.Rate, Start: start}
	for seq := uint64(1); ; seq++ {
		at := sched.Next()
		if !at.Before(end) {
			break
		}
		if !load.Wait(ctx.Done(), at) {
			invalid("量測被中斷，送單時段沒有跑完")
			break
		}
		ref := fmt.Sprintf("%s-c%d", c.Run, seq)
		mu.Lock()
		inflight[ref] = true
		mu.Unlock()
		pwg.Add(1)
		go func(seq uint64, at time.Time) {
			defer pwg.Done()
			err := customer.Place(pctx, seq, at)
			mu.Lock()
			delete(inflight, ref)
			if err != nil && placeErr == nil {
				placeErr = err
			}
			mu.Unlock()
		}(seq, at)
	}
	// The sending period lasts until D even when the last slot is earlier.
	if ctx.Err() == nil && !load.Wait(ctx.Done(), end) {
		invalid("量測被中斷，送單時段沒有跑完")
	}
	// Step 1: no new orders.
	mark("停止送新單")

	// Step 2: client requests and retries finish, at most Limits.Client.
	left := waitUntil(clk, c.Limits.Client, 50*time.Millisecond, func(time.Time) int {
		mu.Lock()
		defer mu.Unlock()
		return len(inflight)
	})
	if left > 0 {
		mu.Lock()
		at := clk.Now()
		facts.ClientCancelAt = &at
		for ref := range inflight {
			facts.ClientInFlight = append(facts.ClientInFlight, ref)
		}
		mu.Unlock()
		sort.Strings(facts.ClientInFlight)
		pcancel()
	}
	pwg.Wait()
	if left > 0 {
		mark(fmt.Sprintf("用戶端等待到上限，%d 張列為用戶端在途", len(facts.ClientInFlight)))
	} else {
		mark("用戶端請求都結束")
	}
	if placeErr != nil {
		invalid("下單紀錄寫入失敗：%v", placeErr)
	}

	// Step 3: server-side results settle.
	settle := func(step string) {
		n := waitUntil(clk, c.Limits.Server, c.Limits.Poll, func(now time.Time) int { return pendingSettle(c, now) })
		if n == 0 {
			mark(step + "：伺服器端結果都已確定")
		} else {
			mark(fmt.Sprintf("%s：等待到上限，%d 次嘗試伺服器端結果未知", step, n))
		}
	}
	settle("第 3 步")

	// Step 4: kitchen finishes what is pending, then stops; settle again.
	derr := kitchen.Drain(wctx, c.Limits.Kitchen)
	kcancel()
	kerr := <-kdone
	mark("廚房停止出餐")
	if kerr != nil {
		invalid("出餐紀錄寫入失敗：%v", kerr)
	} else if derr != nil && aw.Err() == nil {
		facts.Notes = append(facts.Notes, "廚房收尾："+derr.Error())
	}
	settle("第 4 步")

	// Step 5: the cursor, in a single statement; then orders and ready rows.
	if v, err := c.audit(wctx, client, "cursor"); err != nil {
		facts.Audit.Errors = append(facts.Audit.Errors, "cursor："+err.Error())
	} else {
		var r struct {
			Cursor *int64 `json:"cursor"`
		}
		if json.Unmarshal(v, &r) != nil || r.Cursor == nil {
			facts.Audit.Errors = append(facts.Audit.Errors, "cursor：回應格式不對")
		} else {
			facts.Audit.Cursor = r.Cursor
			mark("截止游標 C = " + strconv.FormatInt(*r.Cursor, 10))
		}
	}
	if v, err := c.audit(wctx, client, "orders"); err != nil {
		facts.Audit.Errors = append(facts.Audit.Errors, "orders："+err.Error())
	} else if json.Unmarshal(v, &facts.Audit.Orders) != nil || facts.Audit.Orders == nil {
		facts.Audit.Orders = nil
		facts.Audit.Errors = append(facts.Audit.Errors, "orders：回應格式不對")
	}
	if v, err := c.audit(wctx, client, "ready"); err != nil {
		facts.Audit.Errors = append(facts.Audit.Errors, "ready："+err.Error())
	} else if json.Unmarshal(v, &facts.Audit.Ready) != nil || facts.Audit.Ready == nil {
		facts.Audit.Ready = nil
		facts.Audit.Errors = append(facts.Audit.Errors, "ready：回應格式不對")
	}
	return facts.Audit.Cursor
}

func (c *Config) audit(ctx context.Context, client *http.Client, mode string) ([]byte, error) {
	actx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := c.Base + "/api/audit?run=" + url.QueryEscape(c.Run) + "&mode=" + mode
	req, err := http.NewRequestWithContext(actx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return b, nil
}

// ---------------------------------------------------------------- host

// sampleHost samples /proc/stat and /proc/meminfo every interval until ctx
// is done.
func sampleHost(ctx context.Context, interval time.Duration) *HostStats {
	h := &HostStats{MinMemAvailPct: 100}
	prevBusy, prevTotal, err := readCPU()
	if err != nil {
		return &HostStats{Err: err.Error()}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if h.Samples == 0 {
				h.MinMemAvailPct = 0
			}
			return h
		case <-t.C:
		}
		busy, total, err := readCPU()
		if err != nil {
			h.Err = err.Error()
			continue
		}
		if dt := total - prevTotal; dt > 0 {
			if pct := 100 * float64(busy-prevBusy) / float64(dt); pct > h.MaxCPUPct {
				h.MaxCPUPct = pct
			}
		}
		prevBusy, prevTotal = busy, total
		if m, err := readMemAvailPct(); err == nil {
			if m < h.MinMemAvailPct {
				h.MinMemAvailPct = m
			}
		} else {
			h.Err = err.Error()
		}
		h.Samples++
	}
}

func readCPU() (busy, total uint64, err error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, errors.New("/proc/stat: empty")
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, errors.New("/proc/stat: unexpected format")
	}
	for i, s := range fields[1:] {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return 0, 0, err
		}
		if i >= 8 { // guest and guest_nice are already in user/nice
			break
		}
		total += v
		if i != 3 && i != 4 { // idle, iowait
			busy += v
		}
	}
	return busy, total, nil
}

func readMemAvailPct() (float64, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	var total, avail float64
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseFloat(f[1], 64)
		switch f[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	if total == 0 {
		return 0, errors.New("/proc/meminfo: no MemTotal")
	}
	return 100 * avail / total, nil
}

// ---------------------------------------------------------------- probe

// ProbeConfig is `rush-hour probe`.
type ProbeConfig struct {
	Base, Run, Out string
	Duration       time.Duration
	Rate           float64
}

// Probe sends calibration probes for Duration into DIR/probes.jsonl. Any
// record-write failure is returned: the calibration data is then invalid.
func Probe(ctx context.Context, c ProbeConfig) error {
	if err := os.MkdirAll(c.Out, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(c.Out, "probes.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	w := record.NewWriter(f)
	pctx, cancel := context.WithTimeout(ctx, c.Duration)
	defer cancel()
	perr := (&load.Prober{Base: c.Base, RunID: c.Run, Rate: c.Rate, W: w}).Run(pctx)
	return errors.Join(perr, w.Err(), f.Close())
}
