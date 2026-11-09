package server

// These tests build the real app binary and run it as a subprocess, so a
// real SIGTERM reaches the real signal handling (plan A5, design 4.4/4.6).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/jackc/pgx/v5/pgxpool"

	"koupa/app/applog"
	"koupa/internal/testdb"
)

var raceEnabled bool

var (
	buildOnce sync.Once
	buildDir  string
	binPath   string
	buildErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

// appBinary builds koupa/app once per test run (with -race when the tests
// themselves run with it).
func appBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "koupa-app-")
		if buildErr != nil {
			return
		}
		binPath = filepath.Join(buildDir, "koupa")
		args := []string{"build"}
		if raceEnabled {
			args = append(args, "-race")
		}
		args = append(args, "-ldflags", "-X koupa/app/config.Version=v1", "-o", binPath, "koupa/app")
		out, err := exec.Command("go", args...).CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

type proc struct {
	cmd    *exec.Cmd
	base   string
	addr   string
	log    *syncBuf
	stderr *syncBuf
	exited chan struct{}
	exitAt time.Time
	waitEr error
	termAt time.Time
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// startApp runs the app binary against pool's database and waits until
// /healthz is 200.
func startApp(t *testing.T, pool *pgxpool.Pool, env map[string]string) *proc {
	t.Helper()
	bin := appBinary(t)
	addr := freeAddr(t)
	vars := map[string]string{
		"DATABASE_URL": listenDSN(t, pool), "POD_UID": "uid-sub", "POD_NAME": "pod-sub",
		"LISTEN_ADDR": addr, "BROADCAST_POLL_INTERVAL": "200ms",
	}
	for k, v := range env {
		vars[k] = v
	}
	p := &proc{base: "http://" + addr, addr: addr, log: &syncBuf{}, stderr: &syncBuf{}, exited: make(chan struct{})}
	p.cmd = exec.Command(bin)
	// GORACE: a -race binary otherwise sleeps 1s at exit.
	p.cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GORACE=atexit_sleep_ms=0"}
	for k, v := range vars {
		p.cmd.Env = append(p.cmd.Env, k+"="+v)
	}
	p.cmd.Stdout, p.cmd.Stderr = p.log, p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		p.waitEr = p.cmd.Wait()
		p.exitAt = time.Now()
		close(p.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-p.exited:
		default:
			p.cmd.Process.Kill()
			<-p.exited
		}
		if t.Failed() {
			t.Logf("app stdout:\n%s\napp stderr:\n%s", p.log.b.String(), p.stderr.b.String())
		}
	})
	deadline := time.Now().Add(waitTimeout)
	for {
		resp, err := http.Get(p.base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return p
			}
		}
		select {
		case <-p.exited:
			t.Fatalf("app exited during startup: %v\n%s\n%s", p.waitEr, p.log.b.String(), p.stderr.b.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("app never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (p *proc) sigterm(t *testing.T) {
	t.Helper()
	p.termAt = time.Now()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
}

// waitExit waits for the process to end and returns how long after
// SIGTERM it did.
func (p *proc) waitExit(t *testing.T, limit time.Duration) time.Duration {
	t.Helper()
	select {
	case <-p.exited:
	case <-time.After(limit):
		t.Fatalf("app still running %v after SIGTERM", limit)
	}
	if p.waitEr != nil {
		t.Errorf("app exit: %v", p.waitEr)
	}
	return p.exitAt.Sub(p.termAt)
}

func (p *proc) lastEvent(t *testing.T) string {
	t.Helper()
	lines := p.log.lines(t)
	if len(lines) == 0 {
		return ""
	}
	ev, _ := lines[len(lines)-1]["event"].(string)
	return ev
}

// indexOf returns the position of the first matching line, or -1.
func indexOf(lines []map[string]any, event string, kv ...any) int {
	for i, l := range lines {
		if len(find([]map[string]any{l}, event, kv...)) == 1 {
			return i
		}
	}
	return -1
}

type sseEvent struct {
	event, id, data, retry string
}

type sseBoard struct {
	events chan sseEvent
	ended  chan error // the stream's final read error (io.EOF on a clean close)
}

// openBoard connects to /events and reads events until the stream ends.
func openBoard(t *testing.T, base, attempt string) *sseBoard {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/events", nil)
	req.Header.Set("X-Koupa-Attempt", attempt)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("board status %d", resp.StatusCode)
	}
	b := &sseBoard{events: make(chan sseEvent, 100), ended: make(chan error, 1)}
	go func() {
		defer resp.Body.Close()
		rd := bufio.NewReader(resp.Body)
		var ev sseEvent
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				b.ended <- err
				close(b.events)
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case line == "":
				if ev != (sseEvent{}) {
					b.events <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, ":"):
			default:
				k, v, _ := strings.Cut(line, ":")
				v = strings.TrimPrefix(v, " ")
				switch k {
				case "event":
					ev.event = v
				case "id":
					ev.id = v
				case "data":
					ev.data = v
				case "retry":
					ev.retry = v
				}
			}
		}
	}()
	t.Cleanup(func() { resp.Body.Close() })
	return b
}

func (b *sseBoard) next(t *testing.T) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-b.events:
		if !ok {
			t.Fatal("stream ended")
		}
		return ev
	case <-time.After(waitTimeout):
		t.Fatal("no event in time")
		return sseEvent{}
	}
}

// postOrder sends an order in the background.
type result struct {
	status int
	body   string
	err    error
}

func postOrder(base, attempt, body string) <-chan result {
	out := make(chan result, 1)
	go func() {
		req, _ := http.NewRequest("POST", base+"/orders", strings.NewReader(body))
		req.Header.Set("X-Koupa-Attempt", attempt)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- result{err: err}
			return
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		out <- result{status: resp.StatusCode, body: string(data), err: err}
	}()
	return out
}

func TestInflightCompletesAfterSigterm(t *testing.T) {
	pool := testdb.New(t)
	p := startApp(t, pool, map[string]string{"GRACEFUL_SHUTDOWN": "1", "SHUTDOWN_TIMEOUT": "10s", "LOCK_TIMEOUT": "30s"})
	release := holdMenuLock(t, pool, 1)
	res := postOrder(p.base, "inflight-1", orderBody("if-1", 1, 1))
	awaitLog(t, p.log, applog.EvRequestReceived, "attempt", "inflight-1")
	awaitLockWaiter(t, pool)

	p.sigterm(t)
	awaitLog(t, p.log, applog.EvSigterm)
	time.Sleep(300 * time.Millisecond)
	select {
	case <-p.exited:
		t.Fatal("app exited with a request in flight")
	default:
	}
	release()

	r := <-res
	if r.err != nil || r.status != 201 || !strings.Contains(r.body, `"order_id"`) {
		t.Fatalf("in-flight order: %+v", r)
	}
	if d := p.waitExit(t, 10*time.Second); d > 5*time.Second {
		t.Errorf("exit took %v", d)
	}
	lines := p.log.lines(t)
	end := find(lines, applog.EvRequestEnd, "attempt", "inflight-1")
	if len(end) != 1 || end[0]["status"] != float64(201) || end[0]["tx_outcome"] != "committed" {
		t.Errorf("request_end = %v", end)
	}
	if indexOf(lines, applog.EvSigterm) > indexOf(lines, applog.EvRequestEnd, "attempt", "inflight-1") {
		t.Error("request_end logged before sigterm")
	}
	if len(find(lines, applog.EvShutdownTimeout)) != 0 {
		t.Error("unexpected shutdown_timeout")
	}
	if ev := p.lastEvent(t); ev != applog.EvExit {
		t.Errorf("last log event %q, want %q", ev, applog.EvExit)
	}
}

func TestBoardsGetShutdownEvent(t *testing.T) {
	pool := testdb.New(t)
	p := startApp(t, pool, map[string]string{"GRACEFUL_SHUTDOWN": "1", "SHUTDOWN_TIMEOUT": "10s", "SSE_RETRY": "1500ms"})
	var boards []*sseBoard
	for i := 0; i < 3; i++ {
		b := openBoard(t, p.base, fmt.Sprintf("board-%d", i))
		if ev := b.next(t); ev.event != "hello" {
			t.Fatalf("board %d first event %+v", i, ev)
		}
		boards = append(boards, b)
	}
	p.sigterm(t)
	for i, b := range boards {
		ev := b.next(t)
		if ev.event != "shutdown" || ev.retry != "1500" {
			t.Errorf("board %d got %+v, want shutdown with retry 1500", i, ev)
		}
		select {
		case err := <-b.ended:
			if !errors.Is(err, io.EOF) {
				t.Errorf("board %d ended with %v, want clean EOF", i, err)
			}
		case <-time.After(waitTimeout):
			t.Errorf("board %d not closed after shutdown", i)
		}
	}
	if d := p.waitExit(t, 10*time.Second); d > 3*time.Second {
		t.Errorf("exit took %v with only boards connected", d)
	}
	lines := p.log.lines(t)
	if n := len(find(lines, applog.EvBoardClosed, "reason", "shutdown")); n != 3 {
		t.Errorf("board_closed shutdown = %d, want 3", n)
	}
	if ev := p.lastEvent(t); ev != applog.EvExit {
		t.Errorf("last log event %q", ev)
	}
}

func TestNewConnRefusedAfterSigterm(t *testing.T) {
	pool := testdb.New(t)
	p := startApp(t, pool, map[string]string{"GRACEFUL_SHUTDOWN": "1", "SHUTDOWN_TIMEOUT": "10s", "LOCK_TIMEOUT": "30s"})
	release := holdMenuLock(t, pool, 1)
	res := postOrder(p.base, "hold-1", orderBody("nc-1", 1, 1))
	awaitLockWaiter(t, pool)

	p.sigterm(t)
	awaitLog(t, p.log, applog.EvSigterm)
	refused := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", p.addr, time.Second)
		if err != nil {
			if !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatalf("dial after SIGTERM: %v, want connection refused", err)
			}
			refused = true
			break
		}
		c.Close()
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-p.exited:
		t.Fatal("app exited before the check; the in-flight request should hold it")
	default:
	}
	if !refused {
		t.Fatal("new connections still accepted 2s after SIGTERM")
	}
	if _, err := http.Get(p.base + "/api/whoami"); err == nil {
		t.Error("new request after SIGTERM succeeded")
	}
	release()
	if r := <-res; r.status != 201 {
		t.Errorf("in-flight order: %+v", r)
	}
	p.waitExit(t, 10*time.Second)
}

func TestBlockedBoardDoesNotDelayOthers(t *testing.T) {
	pool := testdb.New(t)
	// Enough backlog that a board resuming from 0 fills any socket buffer.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO orders (number, total, status, ready_seq, ready_at, ready_attempt)
		 SELECT 1, 0, 'ready', nextval('ready_seq_seq'), now(), 'bulk' FROM generate_series(1, 150000)`); err != nil {
		t.Fatal(err)
	}
	p := startApp(t, pool, map[string]string{
		"GRACEFUL_SHUTDOWN": "1", "SHUTDOWN_TIMEOUT": "6s", "SSE_WRITE_DEADLINE": "2s", "RESUME_FROM_LAST_ID": "1",
	})
	a := openBoard(t, p.base, "reader-a")
	b := openBoard(t, p.base, "reader-b")
	for _, bd := range []*sseBoard{a, b} {
		if ev := bd.next(t); ev.event != "hello" {
			t.Fatalf("first event %+v", ev)
		}
	}
	stuck := dialNoRead(t, p.addr)
	defer stuck.Close()
	if _, err := io.WriteString(stuck, "GET /events HTTP/1.1\r\nHost: x\r\nX-Koupa-Attempt: stuck\r\nLast-Event-ID: 0\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	awaitBuffersFull(t, stuck)

	p.sigterm(t)
	for name, bd := range map[string]*sseBoard{"a": a, "b": b} {
		select {
		case ev := <-bd.events:
			if ev.event != "shutdown" {
				t.Errorf("board %s got %+v", name, ev)
			}
		case <-time.After(time.Second):
			t.Errorf("board %s: no shutdown within 1s of SIGTERM", name)
		}
	}
	d := p.waitExit(t, 10*time.Second)
	if d > 6*time.Second {
		t.Errorf("exit %v after SIGTERM, SHUTDOWN_TIMEOUT 6s", d)
	}
	lines := p.log.lines(t)
	si := indexOf(lines, applog.EvSigterm)
	bi := indexOf(lines, applog.EvBoardClosed, "attempt", "stuck", "reason", "write_error")
	if bi < 0 || bi < si {
		t.Fatalf("stuck board: board_closed write_error at line %d, sigterm at %d; it must still be blocked at SIGTERM", bi, si)
	}
	if sent := lines[bi]["sent"].(float64); sent >= 150000 {
		t.Errorf("stuck board sent its whole backlog (%v); it was never blocked", sent)
	}
	// Its blocked write's 2s deadline was set before SIGTERM, so it ends
	// within 2s of it (the exit itself may lag up to Shutdown's 500ms poll).
	if gap := logTime(t, lines[bi]).Sub(logTime(t, lines[si])); gap > 2*time.Second {
		t.Errorf("stuck board ended %v after SIGTERM, want under its 2s write deadline", gap)
	}
	if len(find(lines, applog.EvShutdownTimeout)) != 0 {
		t.Error("unexpected shutdown_timeout")
	}
	t.Logf("exit %v after SIGTERM", d)
}

func TestStuckRequestExitsAtTimeout(t *testing.T) {
	pool := testdb.New(t)
	p := startApp(t, pool, map[string]string{"GRACEFUL_SHUTDOWN": "1", "SHUTDOWN_TIMEOUT": "2s", "LOCK_TIMEOUT": "50s"})
	holdMenuLock(t, pool, 1)
	res := postOrder(p.base, "stuck-1", orderBody("st-1", 1, 1))
	awaitLockWaiter(t, pool)

	p.sigterm(t)
	d := p.waitExit(t, 10*time.Second)
	if d < 2*time.Second || d > 4*time.Second {
		t.Errorf("exit %v after SIGTERM, want about SHUTDOWN_TIMEOUT 2s", d)
	}
	if r := <-res; r.err == nil {
		t.Errorf("stuck request got a response: %+v", r)
	}
	lines := p.log.lines(t)
	to := find(lines, applog.EvShutdownTimeout)
	if len(to) != 1 || to[0]["inflight"] != float64(1) {
		t.Errorf("shutdown_timeout = %v", to)
	}
	if len(find(lines, applog.EvRequestEnd, "attempt", "stuck-1")) != 0 {
		t.Error("stuck request logged request_end")
	}
	if ev := p.lastEvent(t); ev != applog.EvExit {
		t.Errorf("last log event %q", ev)
	}
}

func TestStep0ExitsImmediately(t *testing.T) {
	pool := testdb.New(t)
	p := startApp(t, pool, map[string]string{"LOCK_TIMEOUT": "30s"})
	board := openBoard(t, p.base, "b0")
	if ev := board.next(t); ev.event != "hello" {
		t.Fatalf("first event %+v", ev)
	}
	release := holdMenuLock(t, pool, 1)
	res := postOrder(p.base, "step0-1", orderBody("s0-1", 1, 1))
	awaitLog(t, p.log, applog.EvRequestReceived, "attempt", "step0-1")
	awaitLockWaiter(t, pool)

	p.sigterm(t)
	if d := p.waitExit(t, 5*time.Second); d > time.Second {
		t.Errorf("exit took %v, want immediate", d)
	}
	release()
	if r := <-res; r.err == nil {
		t.Errorf("in-flight request got a response: %+v", r)
	}
	for ev := range board.events {
		if ev.event == "shutdown" {
			t.Error("step 0 board got a shutdown event")
		}
	}
	lines := p.log.lines(t)
	if ev := p.lastEvent(t); ev != applog.EvSigterm {
		t.Errorf("last log event %q, want %q", ev, applog.EvSigterm)
	}
	if n := len(find(lines, applog.EvSigterm)); n != 1 {
		t.Errorf("%d sigterm lines", n)
	}
	if len(find(lines, applog.EvRequestEnd, "attempt", "step0-1")) != 0 {
		t.Error("in-flight request logged request_end")
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM orders WHERE client_ref = 's0-1'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("orders for interrupted request: %d %v", n, err)
	}
}

// awaitBuffersFull waits until the bytes queued in both directions of
// conn stop growing: the client's unread receive queue (TIOCINQ) and the
// server socket's send queue (tx_queue in /proc/net/tcp). The server's
// write to it is then blocked.
func awaitBuffersFull(t *testing.T, conn net.Conn) {
	t.Helper()
	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	recvQ := func() int {
		var n int32
		var errno syscall.Errno
		if err := raw.Control(func(fd uintptr) {
			_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCINQ, uintptr(unsafe.Pointer(&n)))
		}); err != nil || errno != 0 {
			t.Fatalf("TIOCINQ: %v %v", err, errno)
		}
		return int(n)
	}
	local := procAddr(t, conn.RemoteAddr().String())
	remote := procAddr(t, conn.LocalAddr().String())
	sendQ := func() int {
		data, err := os.ReadFile("/proc/net/tcp")
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) > 4 && f[1] == local && f[2] == remote {
				tx, _, _ := strings.Cut(f[4], ":")
				n, err := strconv.ParseInt(tx, 16, 64)
				if err != nil {
					t.Fatal(err)
				}
				return int(n)
			}
		}
		t.Fatalf("server socket %s->%s not in /proc/net/tcp", local, remote)
		return 0
	}
	type q struct{ in, out int }
	last, stable := q{-1, -1}, 0
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		cur := q{recvQ(), sendQ()}
		if cur.in > 0 && cur.out > 0 && cur == last {
			stable++
			if stable >= 6 { // unchanged for 300ms
				t.Logf("buffers full: client receive queue %d bytes, server send queue %d bytes", cur.in, cur.out)
				return
			}
		} else {
			stable = 0
		}
		last = cur
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("buffers never settled (last %+v)", last)
}

// procAddr formats an IPv4 host:port as /proc/net/tcp does.
func procAddr(t *testing.T, hostport string) string {
	t.Helper()
	ap, err := netip.ParseAddrPort(hostport)
	if err != nil || !ap.Addr().Is4() {
		t.Fatalf("addr %q: %v", hostport, err)
	}
	b := ap.Addr().As4()
	return fmt.Sprintf("%02X%02X%02X%02X:%04X", b[3], b[2], b[1], b[0], ap.Port())
}

func logTime(t *testing.T, l map[string]any) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, l["ts"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// TestSigtermDuringStartup: the database accepts TCP but never answers,
// so NewPool blocks; SIGTERM must still be handled at once.
func TestSigtermDuringStartup(t *testing.T) {
	for _, graceful := range []bool{false, true} {
		t.Run(fmt.Sprintf("graceful=%v", graceful), func(t *testing.T) {
			if testing.Short() {
				t.Skip("builds the app binary")
			}
			bin := appBinary(t)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			accepted := make(chan net.Conn, 4)
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					accepted <- c // hold it open, never answer
				}
			}()
			defer func() {
				for len(accepted) > 0 {
					(<-accepted).Close()
				}
			}()
			p := &proc{log: &syncBuf{}, stderr: &syncBuf{}, exited: make(chan struct{})}
			p.cmd = exec.Command(bin)
			p.cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GORACE=atexit_sleep_ms=0",
				"DATABASE_URL=postgres://koupa:koupa@" + ln.Addr().String() + "/koupa?sslmode=disable",
				"LISTEN_ADDR=" + freeAddr(t), "POD_UID=uid-start"}
			if graceful {
				p.cmd.Env = append(p.cmd.Env, "GRACEFUL_SHUTDOWN=1")
			}
			p.cmd.Stdout, p.cmd.Stderr = p.log, p.stderr
			if err := p.cmd.Start(); err != nil {
				t.Fatal(err)
			}
			go func() {
				p.waitEr = p.cmd.Wait()
				p.exitAt = time.Now()
				close(p.exited)
			}()
			t.Cleanup(func() {
				select {
				case <-p.exited:
				default:
					p.cmd.Process.Kill()
					<-p.exited
				}
				if t.Failed() {
					t.Logf("stdout:\n%s\nstderr:\n%s", p.log.b.String(), p.stderr.b.String())
				}
			})
			select {
			case c := <-accepted:
				accepted <- c
			case <-time.After(waitTimeout):
				t.Fatal("app never connected to the database")
			}
			p.sigterm(t)
			if d := p.waitExit(t, 5*time.Second); d > time.Second {
				t.Errorf("exit %v after SIGTERM during startup", d)
			}
			lines := p.log.lines(t)
			var got []string
			for _, l := range lines {
				got = append(got, l["event"].(string))
			}
			want := []string{applog.EvStartup, applog.EvSigterm}
			if graceful {
				want = append(want, applog.EvExit)
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("events %v, want %v", got, want)
			}
			if s := find(lines, applog.EvSigterm); len(s) != 1 || s[0]["phase"] != "startup" {
				t.Errorf("sigterm line %v", s)
			}
		})
	}
}
