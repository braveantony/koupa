// Package testproxy is a TCP proxy for tests that sits between a PostgreSQL
// client and server. It splits the client's byte stream into frontend
// messages so a hook can act on each one, for example to forward a COMMIT
// and then drop the client side so the client never sees the reply.
//
// Only unencrypted sessions are supported: clients must use sslmode=disable.
package testproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Action tells the proxy what to do with one client message.
type Action int

const (
	// Forward passes the message to the server unchanged.
	Forward Action = iota
	// ForwardThenCutClient passes the message to the server, then closes
	// the client side at once and discards every later server reply on
	// this connection. The server side stays open for cutGrace so the
	// server finishes processing the message.
	ForwardThenCutClient
)

// cutGrace is how long the server side is kept open (draining replies)
// after a ForwardThenCutClient.
const cutGrace = 2 * time.Second

// maxMessage caps a single frontend message so a framing bug fails fast.
const maxMessage = 1 << 30

// Protocol codes of untyped requests that are followed by another untyped
// message (the real StartupMessage).
const (
	sslRequestCode    = 80877103
	gssEncRequestCode = 80877104
)

// TCP is a running proxy. Create it with NewTCP.
type TCP struct {
	upstream string
	ln       net.Listener
	ctx      context.Context // cancelled by Close; aborts pending upstream dials
	cancel   context.CancelFunc

	mu     sync.Mutex
	hook   func(msg []byte) Action
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// NewTCP listens on a random 127.0.0.1 port and proxies every accepted
// connection to upstream (host:port). It panics if it cannot listen, which
// in a test is the most useful failure.
func NewTCP(upstream string) *TCP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("testproxy: listen: %v", err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &TCP{upstream: upstream, ln: ln, ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.accept()
	return p
}

// Addr is the proxy's listen address (host:port).
func (p *TCP) Addr() string { return p.ln.Addr().String() }

// OnClientMessage sets the hook called with each complete client→server
// message, including the type byte and length (the first message of a
// connection has no type byte). The hook must not keep msg. Without a hook
// every message is forwarded.
func (p *TCP) OnClientMessage(f func(msg []byte) Action) {
	p.mu.Lock()
	p.hook = f
	p.mu.Unlock()
}

// Close stops listening, closes every proxied connection and waits for
// all proxy goroutines to finish.
func (p *TCP) Close() {
	p.cancel()
	p.mu.Lock()
	p.closed = true
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	p.ln.Close()
	p.wg.Wait()
}

func (p *TCP) accept() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go p.handle(c)
	}
}

// track registers c so Close can shut it; false means the proxy is closing.
func (p *TCP) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *TCP) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

func (p *TCP) action(msg []byte) Action {
	p.mu.Lock()
	h := p.hook
	p.mu.Unlock()
	if h == nil {
		return Forward
	}
	return h(msg)
}

func (p *TCP) handle(client net.Conn) {
	defer p.wg.Done()
	defer client.Close()
	if !p.track(client) {
		return
	}
	defer p.untrack(client)
	var d net.Dialer
	server, err := d.DialContext(p.ctx, "tcp", p.upstream)
	if err != nil {
		return
	}
	defer server.Close()
	if !p.track(server) {
		return
	}
	defer p.untrack(server)

	c := &cutState{}
	done := make(chan struct{})
	go func() { // server → client
		defer close(done)
		buf := make([]byte, 32<<10)
		for {
			n, err := server.Read(buf)
			if n > 0 {
				// A failed client write needs no action here: the client
				// reader sees the dead connection and closes the server
				// side, and until then replies are just discarded.
				c.writeUnlessCut(client, buf[:n])
			}
			if err != nil {
				client.Close()
				return
			}
		}
	}()

	if p.clientToServer(client, server, c) {
		// Keep draining the server for a moment so it processes the
		// message instead of seeing the connection vanish mid-request.
		server.SetReadDeadline(time.Now().Add(cutGrace))
	} else {
		server.Close()
	}
	<-done
}

// clientToServer copies framed messages until the client goes away (false)
// or the hook asks for a cut (true, client already closed).
func (p *TCP) clientToServer(client, server net.Conn, c *cutState) bool {
	untypedNext := true
	for {
		msg, err := readMessage(client, untypedNext)
		if err != nil {
			return false
		}
		// An SSLRequest or GSSENCRequest is followed by another untyped
		// message; anything else is followed by typed messages.
		untypedNext = untypedNext && len(msg) == 8 &&
			(binary.BigEndian.Uint32(msg[4:]) == sslRequestCode ||
				binary.BigEndian.Uint32(msg[4:]) == gssEncRequestCode)
		act := p.action(msg)
		if act == ForwardThenCutClient {
			// Cut before forwarding: the mutex guarantees no client write
			// is in progress or can start, so no reply can reach the client.
			c.cutAndClose(client)
		}
		if _, err := server.Write(msg); err != nil {
			return false
		}
		if act == ForwardThenCutClient {
			return true
		}
	}
}

// cutState makes the cut decision and server→client writes mutually
// exclusive.
type cutState struct {
	mu  sync.Mutex
	cut bool
}

// writeUnlessCut writes b to client unless the connection was cut, in
// which case b is discarded. Write errors are ignored (see handle).
func (c *cutState) writeUnlessCut(client net.Conn, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.cut {
		_, _ = client.Write(b)
	}
}

func (c *cutState) cutAndClose(client net.Conn) {
	// Closing first unblocks a writeUnlessCut stuck on a client that is
	// not reading, so taking the lock cannot hang.
	client.Close()
	c.mu.Lock()
	c.cut = true
	c.mu.Unlock()
}

// readMessage reads one frontend message. untyped means the message has no
// type byte (StartupMessage, SSLRequest, CancelRequest).
func readMessage(r io.Reader, untyped bool) ([]byte, error) {
	hdr := 5
	if untyped {
		hdr = 4
	}
	head := make([]byte, hdr)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(head[hdr-4:])
	if n < 4 || n > maxMessage {
		return nil, errors.New("testproxy: bad message length")
	}
	msg := make([]byte, hdr+int(n)-4)
	copy(msg, head)
	if _, err := io.ReadFull(r, msg[hdr:]); err != nil {
		return nil, err
	}
	return msg, nil
}
