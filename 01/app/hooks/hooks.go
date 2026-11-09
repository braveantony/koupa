package hooks

import (
	"context"
	"encoding/json"
	"errors"
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

const (
	defaultWait = 30 * time.Second
	hitBuffer   = 1024
)

// point is the state of one named pause point.
type point struct {
	armed  bool
	paused []chan struct{} // closed by release
	hits   chan string     // attempt ids of paused requests, for waiters
}

// Registry is the TEST_HOOKS implementation of Point. arm makes the next
// request that reaches a point pause there; wait reports it; release lets
// every paused request go and disarms the point.
type Registry struct {
	enabled bool
	log     *slog.Logger

	mu      sync.Mutex
	podUID  string
	podName string
	points  map[string]*point
}

var _ Point = (*Registry)(nil)

// New returns a Registry. With enabled=false, Pause returns at once and Run
// opens no port.
func New(enabled bool, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Registry{enabled: enabled, log: log, points: map[string]*point{}}
	for _, n := range names {
		r.points[n] = &point{hits: make(chan string, hitBuffer)}
	}
	return r
}

// SetPod sets the identity reported to waiters.
func (r *Registry) SetPod(uid, name string) {
	r.mu.Lock()
	r.podUID, r.podName = uid, name
	r.mu.Unlock()
}

// Pause blocks while the point is armed (consuming the arming) until release
// or ctx ends.
func (r *Registry) Pause(ctx context.Context, name, attemptID string) {
	if r == nil || !r.enabled {
		return
	}
	r.mu.Lock()
	p, ok := r.points[name]
	if !ok || !p.armed {
		r.mu.Unlock()
		return
	}
	p.armed = false
	rel := make(chan struct{})
	p.paused = append(p.paused, rel)
	r.mu.Unlock()

	r.log.Info(applog.EvHookPaused, "hook", name, "attempt", attemptID)
	select {
	case p.hits <- attemptID:
	default:
	}
	select {
	case <-rel:
	case <-ctx.Done():
	}
}

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
	for _, c := range p.paused {
		close(c)
	}
	p.paused = nil
	// Drop reports of requests that are no longer paused.
	for {
		select {
		case <-p.hits:
			continue
		default:
		}
		break
	}
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
	select {
	case attempt := <-p.hits:
		r.mu.Lock()
		uid, name := r.podUID, r.podName
		r.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"pod_uid": uid, "pod_name": name, "attempt": attempt})
	case <-timer.C:
		http.Error(w, "timeout", http.StatusRequestTimeout)
	case <-q.Context().Done():
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Run serves the control API on addr until ctx ends. When the Registry is
// disabled it returns nil at once and opens no port. The listener is open
// when Run starts serving; a listen error is returned.
func (r *Registry) Run(ctx context.Context, addr string) error {
	if !r.enabled {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: r.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		_ = srv.Close()
		if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
