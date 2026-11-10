package testproxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
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

// fakeUpstream accepts one connection, records everything it receives and,
// framing the stream like the proxy does, replies "r<i>;" to the i-th
// complete message (startup = 0), so every reply names its message.
type fakeUpstream struct {
	ln  net.Listener
	mu  sync.Mutex
	got bytes.Buffer
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeUpstream{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		for i := 0; ; i++ {
			msg, err := readMessage(c, i == 0)
			if err != nil {
				return
			}
			f.mu.Lock()
			f.got.Write(msg)
			f.mu.Unlock()
			if _, err := fmt.Fprintf(c, "r%d;", i); err != nil {
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
	if _, err := io.ReadAtLeast(c, make([]byte, 3), 3); err != nil {
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

	// Step 1: the startup message is forwarded and its reply delivered.
	start := untyped("startup")
	if _, err := c.Write(start); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	reply := make([]byte, 3)
	if _, err := io.ReadFull(c, reply); err != nil || string(reply) != "r0;" {
		t.Fatalf("startup reply = %q, %v; want r0;", reply, err)
	}

	// Step 2: the cut message reaches the upstream, which replies, but the
	// client only sees its side closed with no further bytes.
	q := typed('Q', "commit\x00")
	if _, err := c.Write(q); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(c)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("client side was not closed")
	}
	if bytes.Contains(got, []byte("r1;")) || len(got) != 0 {
		t.Fatalf("client received %q after the cut, want nothing (never r1;)", got)
	}
	want := append(append([]byte(nil), start...), q...)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !bytes.Equal(up.bytes(), want) {
		time.Sleep(10 * time.Millisecond)
	}
	if !bytes.Equal(up.bytes(), want) {
		t.Fatalf("upstream got %q, want %q", up.bytes(), want)
	}
}

// TestTCPCloseDuringPendingDial checks that Close cancels an upstream dial
// that is still pending instead of waiting for the OS connect timeout.
func TestTCPCloseDuringPendingDial(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan error, 1)
	p := newTCP("upstream.invalid:5432", func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(started)
		<-ctx.Done() // a dial that only ends when cancelled
		cancelled <- ctx.Err()
		return nil, ctx.Err()
	})
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream dial never started")
	}

	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on a pending upstream dial")
	}
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dial ctx err = %v, want context.Canceled", err)
		}
	default:
		t.Fatal("Close returned but the dial ctx was not cancelled")
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
