// Command slow-board runs inside the cluster next to one koupa Pod (plan
// A12 timing 5): healthy boards read the SSE stream normally, the stuck
// board stops reading after hello so the App's writes to it really block.
// It connects to the Pod IP directly, never through the ingress, so no
// proxy buffer absorbs the data.
//
// The App cuts a stuck connection when its write deadline (2s) passes, so
// the stuck board opens a fresh connection every --stuck-every (and never
// reads any of them past hello): at any moment one of its writes is
// blocked, including when SIGTERM arrives.
//
// Output: one JSON object per line on stdout,
// {"t","board","event",...}; events: connected, hello, shutdown, eof,
// error, stuck_open.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var outMu sync.Mutex

func emit(board, event string, kv ...any) {
	m := map[string]any{"t": time.Now().UTC().Format(time.RFC3339Nano), "board": board, "event": event}
	for i := 0; i+1 < len(kv); i += 2 {
		m[fmt.Sprint(kv[i])] = kv[i+1]
	}
	outMu.Lock()
	defer outMu.Unlock()
	_ = json.NewEncoder(os.Stdout).Encode(m)
}

func main() {
	target := flag.String("target", "", "Pod IP:port")
	healthy := flag.Int("healthy", 2, "boards that read normally")
	rcvbuf := flag.Int("rcvbuf", 4096, "SO_RCVBUF of the stuck board's sockets")
	every := flag.Duration("stuck-every", time.Second, "how often the stuck board opens a new connection")
	keep := flag.Int("stuck-keep", 5, "stuck connections kept open at most")
	dur := flag.Duration("duration", 5*time.Minute, "run at most this long")
	flag.Parse()
	if *target == "" {
		fmt.Fprintln(os.Stderr, "slow-board: --target required")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	ctx, cancel2 := context.WithTimeout(ctx, *dur)
	defer cancel2()

	var wg sync.WaitGroup
	for i := 1; i <= *healthy; i++ {
		wg.Add(1)
		go func(name string) { defer wg.Done(); healthyBoard(ctx, name, *target) }(fmt.Sprintf("healthy-%d", i))
	}
	wg.Add(1)
	go func() { defer wg.Done(); stuckBoard(ctx, *target, *rcvbuf, *every, *keep) }()
	wg.Wait()
}

// healthyBoard reads every line; it reconnects after the stream ends until
// the Pod refuses (then it keeps trying quietly until ctx ends).
func healthyBoard(ctx context.Context, name, target string) {
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	for conn := 1; ctx.Err() == nil; conn++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+target+"/events", nil)
		req.Header.Set("X-Koupa-Attempt", fmt.Sprintf("slow-board-%s-%d", name, conn))
		resp, err := client.Do(req)
		if err != nil {
			if conn < 1000 {
				emit(name, "error", "conn", conn, "err", err.Error())
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		emit(name, "connected", "conn", conn, "status", resp.StatusCode)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		var event string
		for sc.Scan() {
			l := sc.Text()
			switch {
			case strings.HasPrefix(l, "event: "):
				event = strings.TrimPrefix(l, "event: ")
			case l == "" && event != "":
				if event == "hello" || event == "shutdown" {
					emit(name, event, "conn", conn)
				}
				event = ""
			}
		}
		emit(name, "eof", "conn", conn, "err", fmt.Sprint(sc.Err()))
		resp.Body.Close()
		time.Sleep(200 * time.Millisecond)
	}
}

// stuckBoard opens a connection, reads up to hello, then never reads it
// again; a new one every `every`, keeping at most `keep` open.
func stuckBoard(ctx context.Context, target string, rcvbuf int, every time.Duration, keep int) {
	var open []net.Conn
	defer func() {
		for _, c := range open {
			c.Close()
		}
	}()
	d := net.Dialer{Timeout: 2 * time.Second, Control: func(_, _ string, rc syscall.RawConn) error {
		// Before connect, so the advertised window is small from the start.
		var serr error
		_ = rc.Control(func(fd uintptr) { serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvbuf) })
		return serr
	}}
	t := time.NewTicker(every)
	defer t.Stop()
	for n := 1; ; n++ {
		if c := stuckOne(ctx, d, target, n); c != nil {
			open = append(open, c)
			if len(open) > keep {
				open[0].Close()
				open = open[1:]
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func stuckOne(ctx context.Context, d net.Dialer, target string, n int) net.Conn {
	c, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		if n < 1000 {
			emit("stuck", "error", "conn", n, "err", err.Error())
		}
		return nil
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET /events HTTP/1.1\r\nHost: koupa\r\nX-Koupa-Attempt: slow-board-stuck-%d\r\n\r\n", n)
	// Read byte by byte up to the hello block, so nothing past it is
	// pulled out of the socket.
	var line []byte
	b := make([]byte, 1)
	for {
		if _, err := c.Read(b); err != nil {
			emit("stuck", "error", "conn", n, "err", err.Error())
			c.Close()
			return nil
		}
		line = append(line, b[0])
		if strings.HasSuffix(string(line), "event: hello\n") {
			break
		}
	}
	_ = c.SetDeadline(time.Time{})
	emit("stuck", "stuck_open", "conn", n)
	return c
}
