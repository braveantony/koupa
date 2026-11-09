package hooks

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"koupa/app/applog"
)

// Pause point names (plan A12). Only these are accepted by the control API.
const (
	OrderBeforeCommit = "order.before_commit"
	OrderAfterCommit  = "order.after_commit"
	RequestMid        = "request.mid"
	SSEAfterHello     = "sse.after_hello"
)

var names = []string{OrderBeforeCommit, OrderAfterCommit, RequestMid, SSEAfterHello}

const defaultWait = 30 * time.Second

// pause is one request held at a point.
type pause struct {
	attempt  string
	ctx      context.Context
	rel      chan struct{} // closed by release
	reported bool          // already returned by a wait
}

// point is the state of one named pause point.
type point struct {
	armed   bool
	pauses  []*pause      // live pauses, oldest first
	changed chan struct{} // closed and replaced when pauses change
}

func (p *point) notify() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// Registry is the TEST_HOOKS implementation of Point. arm makes every
// request that reaches a point pause there until release; wait reports each
// live pause once, oldest first; release lets all paused requests go,
// logging one line per request, and disarms the point.
type Registry struct {
	enabled bool
	log     *slog.Logger

	mu      sync.Mutex
	podUID  string
	podName string
	points  map[string]*point
}

var _ Point = (*Registry)(nil)

// New returns a Registry. With enabled=false, Pause returns at once and
// Start opens no port.
func New(enabled bool, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Registry{enabled: enabled, log: log, points: map[string]*point{}}
	for _, n := range names {
		r.points[n] = &point{changed: make(chan struct{})}
	}
	return r
}

// SetPod sets the identity reported to waiters.
func (r *Registry) SetPod(uid, name string) {
	r.mu.Lock()
	r.podUID, r.podName = uid, name
	r.mu.Unlock()
}

// Pause blocks while the point is armed until release or ctx ends. A pause
// whose ctx ends is removed and never reported afterwards.
func (r *Registry) Pause(ctx context.Context, name, attemptID string) {
	if r == nil || !r.enabled {
		return
	}
	r.mu.Lock()
	p, ok := r.points[name]
	// A request whose ctx has already ended never becomes a pause.
	if !ok || !p.armed || ctx.Err() != nil {
		r.mu.Unlock()
		return
	}
	ps := &pause{attempt: attemptID, ctx: ctx, rel: make(chan struct{})}
	p.pauses = append(p.pauses, ps)
	p.notify()
	// Logged under the lock so release (which also holds it) logs after.
	r.log.Info(applog.EvHookPaused, "hook", name, "attempt", attemptID)
	r.mu.Unlock()

	select {
	case <-ps.rel:
	case <-ctx.Done():
		if cancelHook != nil {
			cancelHook()
		}
		r.mu.Lock()
		for i, q := range p.pauses {
			if q == ps {
				p.pauses = append(p.pauses[:i], p.pauses[i+1:]...)
				p.notify()
				break
			}
		}
		r.mu.Unlock()
	}
}

// cancelHook is a test seam run before a cancelled pause removes itself.
var cancelHook func()

func (r *Registry) arm(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.points[name]
	if ok {
		p.armed = true
	}
	return ok
}

func (r *Registry) release(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.points[name]
	if !ok {
		return false
	}
	p.armed = false
	for _, ps := range p.pauses {
		r.log.Info(applog.EvHookReleased, "hook", name, "attempt", ps.attempt)
		close(ps.rel)
	}
	p.pauses = nil
	p.notify()
	return true
}

// Handler is the control API:
//
//	POST /hooks/{name}/arm
//	POST /hooks/{name}/release
//	GET  /hooks/{name}/wait?timeout=30s   -> {"pod_uid","pod_name","attempt"}
//
// An unknown name is 404; a wait that times out is 408.
func (r *Registry) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/{name}/arm", func(w http.ResponseWriter, q *http.Request) {
		if !r.arm(q.PathValue("name")) {
			http.Error(w, "unknown hook", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "armed"})
	})
	mux.HandleFunc("POST /hooks/{name}/release", func(w http.ResponseWriter, q *http.Request) {
		if !r.release(q.PathValue("name")) {
			http.Error(w, "unknown hook", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
	})
	mux.HandleFunc("GET /hooks/{name}/wait", r.handleWait)
	return mux
}

func (r *Registry) handleWait(w http.ResponseWriter, q *http.Request) {
	r.mu.Lock()
	p, ok := r.points[q.PathValue("name")]
	r.mu.Unlock()
	if !ok {
		http.Error(w, "unknown hook", http.StatusNotFound)
		return
	}
	timeout := defaultWait
	if s := q.URL.Query().Get("timeout"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 {
			http.Error(w, "bad timeout", http.StatusBadRequest)
			return
		}
		timeout = d
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		r.mu.Lock()
		// Drop pauses whose request already ended; they are never reported.
		live := p.pauses[:0]
		for _, ps := range p.pauses {
			if ps.ctx.Err() == nil {
				live = append(live, ps)
			}
		}
		p.pauses = live
		for _, ps := range p.pauses {
			if !ps.reported {
				ps.reported = true
				uid, name := r.podUID, r.podName
				r.mu.Unlock()
				writeJSON(w, http.StatusOK, map[string]string{"pod_uid": uid, "pod_name": name, "attempt": ps.attempt})
				return
			}
		}
		changed := p.changed
		r.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			http.Error(w, "timeout", http.StatusRequestTimeout)
			return
		case <-q.Context().Done():
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Start opens the control port and serves it in the background until ctx
// ends. When the Registry is disabled it returns nil and opens no port. The
// listener is open when Start returns; a listen error is returned.
func (r *Registry) Start(ctx context.Context, addr string) error {
	if !r.enabled {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: r.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	go func() { <-ctx.Done(); _ = srv.Close() }()
	return nil
}
