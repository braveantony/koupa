package testproxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"koupa/internal/testdb"
)

// typed builds a regular frontend message: type byte, int32 length, payload.
func typed(t byte, payload string) []byte {
	b := []byte{t, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(b[1:], uint32(4+len(payload)))
	return append(b, payload...)
}

// untyped builds a startup-style message: int32 length, payload.
func untyped(payload string) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(4+len(payload)))
	return append(b, payload...)
}

// fakeUpstream accepts one connection, records everything it receives and
// replies "ok" after each read.
type fakeUpstream struct {
	ln  net.Listener
	mu  sync.Mutex
	got bytes.Buffer
	eof chan struct{}
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeUpstream{ln: ln, eof: make(chan struct{})}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		defer close(f.eof)
		buf := make([]byte, 1024)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				f.mu.Lock()
				f.got.Write(buf[:n])
				f.mu.Unlock()
				c.Write([]byte("ok"))
			}
			if err != nil {
				return
			}
		}
	}()
	return f
}

func (f *fakeUpstream) bytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.got.Bytes()...)
}

func TestTCPSplitsMessagesAndForwards(t *testing.T) {
	up := newFakeUpstream(t)
	p := NewTCP(up.ln.Addr().String())
	defer p.Close()
	var mu sync.Mutex
	var seen [][]byte
	p.OnClientMessage(func(msg []byte) Action {
		mu.Lock()
		seen = append(seen, append([]byte(nil), msg...))
		mu.Unlock()
		return Forward
	})

	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msgs := [][]byte{untyped("startup"), typed('Q', "select 1\x00"), typed('X', "")}
	var all []byte
	for _, m := range msgs {
		all = append(all, m...)
	}
	// Write in odd-sized chunks so the proxy must reassemble message bounds.
	for i := 0; i < len(all); i += 3 {
		end := min(i+3, len(all))
		if _, err := c.Write(all[i:end]); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAtLeast(c, make([]byte, 2), 2); err != nil {
		t.Fatalf("no reply through proxy: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !bytes.Equal(up.bytes(), all) {
		time.Sleep(10 * time.Millisecond)
	}
	if !bytes.Equal(up.bytes(), all) {
		t.Fatalf("upstream got %q, want %q", up.bytes(), all)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(msgs) {
		t.Fatalf("hook saw %d messages, want %d: %q", len(seen), len(msgs), seen)
	}
	for i := range msgs {
		if !bytes.Equal(seen[i], msgs[i]) {
			t.Errorf("message %d = %q, want %q", i, seen[i], msgs[i])
		}
	}
}

func TestTCPForwardThenCutClient(t *testing.T) {
	up := newFakeUpstream(t)
	p := NewTCP(up.ln.Addr().String())
	defer p.Close()
	p.OnClientMessage(func(msg []byte) Action {
		if msg[0] == 'Q' {
			return ForwardThenCutClient
		}
		return Forward
	})
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	q := typed('Q', "commit\x00")
	if _, err := c.Write(append(untyped("startup"), q...)); err != nil {
		t.Fatal(err)
	}
	// The client side must be closed; any bytes it reads must not include
	// the upstream's reply to the cut message.
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(c)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("client side was not closed")
	}
	if strings.Count(string(got), "ok") > 1 {
		t.Fatalf("client received the reply after the cut: %q", got)
	}
	// The cut message itself still reached the upstream.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !bytes.HasSuffix(up.bytes(), q) {
		time.Sleep(10 * time.Millisecond)
	}
	if !bytes.HasSuffix(up.bytes(), q) {
		t.Fatalf("upstream got %q, want it to end with %q", up.bytes(), q)
	}
}

// TestTCPWithPostgres runs a real pgx session through the proxy and records
// how pgx frames COMMIT on the wire.
func TestTCPWithPostgres(t *testing.T) {
	pool := testdb.New(t)
	cc := pool.Config().ConnConfig
	p := NewTCP(net.JoinHostPort(cc.Host, strconv.Itoa(int(cc.Port))))
	defer p.Close()
	var mu sync.Mutex
	var commitTypes []byte
	p.OnClientMessage(func(msg []byte) Action {
		if len(msg) > 5 && (msg[0] == 'Q' || msg[0] == 'P') &&
			strings.Contains(strings.ToLower(string(msg[5:])), "commit") {
			mu.Lock()
			commitTypes = append(commitTypes, msg[0])
			mu.Unlock()
		}
		return Forward
	})
	u := url.URL{Scheme: "postgres", User: url.UserPassword(cc.User, cc.Password),
		Host: p.Addr(), Path: "/" + cc.Database, RawQuery: "sslmode=disable"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var one int
	if err := tx.QueryRow(ctx, "SELECT $1::int", 1).Scan(&one); err != nil || one != 1 {
		t.Fatalf("query through proxy: %d %v", one, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("COMMIT message types seen: %q", commitTypes)
	if len(commitTypes) != 1 {
		t.Fatalf("saw %d COMMIT messages, want 1", len(commitTypes))
	}
}
