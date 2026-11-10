package server

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
	"time"

	"koupa/app/applog"
	"koupa/app/store"
)

func TestCommitUnknownLogged(t *testing.T) {
	buf := &syncBuf{}
	s := New(testConfig("v1"), Deps{Log: applog.New(buf, "uid-test", "p", "v1")})
	sent := time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)
	h := s.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordTx(r, store.TxResult{Outcome: store.TxCommitUnknown, CommitSentAt: sent})
		writeError(w, http.StatusServiceUnavailable, "commit outcome unknown")
	}))
	ts := httptest.NewServer(h)
	defer ts.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/orders", nil)
	req.Header.Set("X-Koupa-Attempt", "cu-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	end := awaitLog(t, buf, applog.EvRequestEnd, "attempt", "cu-1")
	if end["tx_outcome"] != "commit_unknown" || end["commit_sent_at"] != sent.Format(time.RFC3339Nano) || end["status"] != float64(503) {
		t.Errorf("request_end = %v", end)
	}
	cu := awaitLog(t, buf, applog.EvCommitUnknown, "attempt", "cu-1")
	if cu["commit_sent_at"] != sent.Format(time.RFC3339Nano) || cu["path"] != "/orders" {
		t.Errorf("commit_unknown = %v", cu)
	}
}

func TestWrappedWriterKeepsFlusher(t *testing.T) {
	buf := &syncBuf{}
	s := New(testConfig("v1"), Deps{Log: applog.New(buf, "uid-test", "p", "v1")})
	res := make(chan error, 3)
	h := s.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			res <- errors.New("not a Flusher")
		}
		rc := http.NewResponseController(w)
		res <- rc.SetWriteDeadline(time.Now().Add(time.Second))
		io.WriteString(w, "data: x\n\n")
		res <- rc.Flush()
	}))
	ts := httptest.NewServer(h)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	for i := 0; i < 2; i++ {
		if err := <-res; err != nil {
			t.Fatal(err)
		}
	}
	end := awaitLog(t, buf, applog.EvRequestEnd, "path", "/events")
	if end["status"] != float64(200) || end["flush_err"] != "" || end["write_err"] != "" {
		t.Errorf("request_end = %v", end)
	}
}

// TestWriteDeadlineRawTCP is plan A12 item 5's last check: through the
// app's middleware, SetWriteDeadline succeeds and a write blocked by a
// client that never reads fails with a timeout once the deadline passes.
func TestWriteDeadlineRawTCP(t *testing.T) {
	buf := &syncBuf{}
	s := New(testConfig("v1"), Deps{Log: applog.New(buf, "uid-test", "p", "v1")})
	const deadline = 300 * time.Millisecond
	type result struct {
		setErr     error
		writeErr   error
		blockedFor time.Duration
		written    int
	}
	done := make(chan result, 1)
	h := s.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		w.WriteHeader(200)
		chunk := make([]byte, 64<<10)
		var res result
		for i := 0; i < 2000; i++ {
			if err := rc.SetWriteDeadline(time.Now().Add(deadline)); err != nil {
				res.setErr = err
				break
			}
			start := time.Now()
			_, err := w.Write(chunk)
			if err == nil {
				err = rc.Flush()
			}
			if err != nil {
				res.writeErr, res.blockedFor = err, time.Since(start)
				break
			}
			res.written += len(chunk)
		}
		done <- res
	}))
	ts := httptest.NewServer(h)
	defer ts.Close()

	conn := dialNoRead(t, ts.Listener.Addr().String())
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET /events HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(waitTimeout):
		t.Fatal("handler never returned: write deadline did not fire")
	}
	if r.setErr != nil {
		t.Fatalf("SetWriteDeadline: %v", r.setErr)
	}
	var ne net.Error
	if r.writeErr == nil || !(errors.Is(r.writeErr, os.ErrDeadlineExceeded) || (errors.As(r.writeErr, &ne) && ne.Timeout())) {
		t.Fatalf("blocked write: err=%v after %d bytes, want a timeout", r.writeErr, r.written)
	}
	if r.blockedFor < deadline/2 || r.blockedFor > deadline+2*time.Second {
		t.Errorf("blocked write returned after %v, deadline %v", r.blockedFor, deadline)
	}
	end := awaitLog(t, buf, applog.EvRequestEnd, "path", "/events")
	if end["write_err"] == "" && end["flush_err"] == "" {
		t.Errorf("request_end has no write/flush error: %v", end)
	}
	t.Logf("wrote %d bytes before blocking; blocked write failed after %v: %v", r.written, r.blockedFor, r.writeErr)
}

// dialNoRead connects with a tiny receive buffer and never reads, so the
// server's writes block quickly.
func dialNoRead(t *testing.T, addr string) net.Conn {
	t.Helper()
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
		}); err != nil {
			return err
		}
		return serr
	}}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}
