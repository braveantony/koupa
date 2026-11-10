package testproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
)

// HTTP is an HTTP/1.1 reverse proxy for tests that sits between an ingress
// (Envoy) and the App. Rules set with HoldNext, DropNext, HoldResponseNext
// and CutResponseNext act on the next matching request each (one rule per
// request, in the order the rules were set); other requests pass through.
// Create it with NewHTTP.
//
// Keep-alives are off on both sides, so every request has a connection of
// its own: resetting a client connection affects exactly one request, and
// an ingress retry always opens a new connection (which Refuse can refuse).
//
// A request a rule took is forwarded with a context of its own, so it
// still reaches the upstream after its client has gone; that is how a
// late arrival is reproduced. Pass-through requests are cancelled when
// their client goes away, like on a real connection.
type HTTP struct {
	upstream string
	addr     string
	srv      *http.Server
	tr       *http.Transport
	ctx      context.Context // cancelled by Close
	cancel   context.CancelFunc

	mu     sync.Mutex
	ln     net.Listener // nil while refusing
	rules  []*httpRule
	closed bool
	wg     sync.WaitGroup // handlers and Serve loops
}

type ruleKind int

const (
	holdRequest  ruleKind = iota // client waits; forward on release
	dropClient                   // reset client now; forward on release
	holdResponse                 // forward now; relay the response on release
	cutResponse                  // forward now; reset client instead of relaying
)

type httpRule struct {
	kind   ruleKind
	match  func(*http.Request) bool
	before func()
	gate   chan struct{}
	once   sync.Once
}

func (r *httpRule) release() { r.once.Do(func() { close(r.gate) }) }

// NewHTTP listens on a random 127.0.0.1 port and proxies every request to
// upstream (host:port). It panics if it cannot listen.
func NewHTTP(upstream string) *HTTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("testproxy: listen: %v", err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &HTTP{
		upstream: upstream, addr: ln.Addr().String(), ln: ln, ctx: ctx, cancel: cancel,
		tr: &http.Transport{DisableKeepAlives: true, DisableCompression: true},
	}
	p.srv = &http.Server{Handler: http.HandlerFunc(p.handle)}
	p.srv.SetKeepAlivesEnabled(false)
	p.serve(ln)
	return p
}

// Addr is the proxy's listen address (host:port). It stays the same across
// Refuse and resume.
func (p *HTTP) Addr() string { return p.addr }

func (p *HTTP) serve(ln net.Listener) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		_ = p.srv.Serve(ln)
	}()
}

func (p *HTTP) add(kind ruleKind, match func(*http.Request) bool, before func()) *httpRule {
	r := &httpRule{kind: kind, match: match, before: before, gate: make(chan struct{})}
	p.mu.Lock()
	p.rules = append(p.rules, r)
	p.mu.Unlock()
	return r
}

// HoldNext holds the next request matching match: its client waits and
// nothing is sent upstream until release is called; then the request is
// forwarded and the response relayed (if the client is still there).
// release may be called more than once.
func (p *HTTP) HoldNext(match func(*http.Request) bool) (release func()) {
	return p.add(holdRequest, match, nil).release
}

// DropNext takes the next request matching match, calls before (when not
// nil), then resets the client connection at once (TCP RST), so the
// client sees the connection reset with no response. The request is kept
// and forwarded upstream only when release is called; its response is
// discarded.
func (p *HTTP) DropNext(match func(*http.Request) bool, before func()) (release func()) {
	return p.add(dropClient, match, before).release
}

// HoldResponseNext forwards the next request matching match at once but
// relays the upstream response only when release is called. A client that
// gives up meanwhile never gets it.
func (p *HTTP) HoldResponseNext(match func(*http.Request) bool) (release func()) {
	return p.add(holdResponse, match, nil).release
}

// CutResponseNext forwards the next request matching match; once the full
// upstream response has arrived, it resets the client connection instead
// of relaying it. The upstream finishes normally; the client gets nothing.
func (p *HTTP) CutResponseNext(match func(*http.Request) bool) {
	p.add(cutResponse, match, nil)
}

// Refuse stops listening, so new connections are refused, until resume is
// called; resume listens again on the same address. Connections already
// accepted are not affected. resume may be called more than once and
// panics if the address cannot be bound again.
func (p *HTTP) Refuse() (resume func()) {
	p.mu.Lock()
	if p.ln != nil {
		p.ln.Close()
		p.ln = nil
	}
	p.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.closed || p.ln != nil {
				return
			}
			ln, err := net.Listen("tcp", p.addr)
			if err != nil {
				panic(fmt.Sprintf("testproxy: listen again on %s: %v", p.addr, err))
			}
			p.ln = ln
			p.serve(ln)
		})
	}
}

// Close stops the proxy. Requests still held are dropped without being
// forwarded. It waits for every handler to return.
func (p *HTTP) Close() {
	p.mu.Lock()
	p.closed = true
	p.ln = nil
	p.mu.Unlock()
	p.cancel()
	p.srv.Close()
	p.wg.Wait()
	p.tr.CloseIdleConnections()
}

// take removes and returns the first rule matching r, or nil.
func (p *HTTP) take(r *http.Request) *httpRule {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, rule := range p.rules {
		if rule.match(r) {
			p.rules = append(p.rules[:i:i], p.rules[i+1:]...)
			return rule
		}
	}
	return nil
}

func (p *HTTP) track() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.wg.Add(1)
	return true
}

// upResponse is a fully read upstream response.
type upResponse struct {
	status int
	header http.Header
	body   []byte
}

// hopHeaders are connection-level headers a proxy must not forward.
var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *HTTP) forward(ctx context.Context, r *http.Request, body []byte) (*upResponse, error) {
	out, err := http.NewRequestWithContext(ctx, r.Method, "http://"+p.upstream+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	out.Header = r.Header.Clone()
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	out.Host = r.Host
	resp, err := p.tr.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &upResponse{status: resp.StatusCode, header: resp.Header, body: b}, nil
}

func relay(w http.ResponseWriter, u *upResponse) {
	for k, v := range u.header {
		w.Header()[k] = v
	}
	for _, h := range hopHeaders {
		w.Header().Del(h)
	}
	w.WriteHeader(u.status)
	_, _ = w.Write(u.body)
}

// resetClient closes the client connection with a TCP RST.
func resetClient(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic("testproxy: response writer cannot be hijacked")
	}
	c, _, err := hj.Hijack()
	if err != nil {
		return
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	c.Close()
}

// wait blocks until the rule is released (true) or the proxy closes.
func (p *HTTP) wait(rule *httpRule) bool {
	select {
	case <-rule.gate:
		return true
	case <-p.ctx.Done():
		return false
	}
}

func (p *HTTP) handle(w http.ResponseWriter, r *http.Request) {
	if !p.track() {
		resetClient(w)
		return
	}
	defer p.wg.Done()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		resetClient(w)
		return
	}
	rule := p.take(r)
	if rule == nil {
		u, err := p.forward(r.Context(), r, body)
		if err != nil {
			http.Error(w, "testproxy: upstream: "+err.Error(), http.StatusBadGateway)
			return
		}
		relay(w, u)
		return
	}
	switch rule.kind {
	case holdRequest:
		if !p.wait(rule) {
			resetClient(w)
			return
		}
		u, err := p.forward(p.ctx, r, body)
		if err != nil {
			resetClient(w)
			return
		}
		relay(w, u)
	case dropClient:
		if rule.before != nil {
			rule.before()
		}
		resetClient(w)
		if p.wait(rule) {
			_, _ = p.forward(p.ctx, r, body)
		}
	case holdResponse:
		u, err := p.forward(p.ctx, r, body)
		if err != nil || !p.wait(rule) {
			resetClient(w)
			return
		}
		relay(w, u)
	case cutResponse:
		_, _ = p.forward(p.ctx, r, body)
		resetClient(w)
	}
}
