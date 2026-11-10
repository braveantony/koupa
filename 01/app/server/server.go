// Package server wires the HTTP routes, the request log middleware and the
// two SIGTERM behaviours (design 4.4 step 0, 4.7 graceful shutdown).
package server

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"koupa/app/board"
	"koupa/app/config"
	"koupa/app/hooks"
)

// Broadcaster is what the server needs from *broadcast.Broadcaster.
type Broadcaster interface {
	board.Registrar
	Ready() <-chan struct{}
	Listening() bool
}

// Deps are the server's collaborators.
type Deps struct {
	Pool        *pgxpool.Pool
	Broadcaster Broadcaster
	Log         *slog.Logger
	// FinalLog writes the process's last line (step 0: sigterm; graceful:
	// exit) and stops all further logging (see applog.Gate). nil uses Log.
	FinalLog *slog.Logger
	// Hooks is the test pause point; nil means hooks.Noop.
	Hooks hooks.Point
	// Pages holds order.html, kitchen.html, board.html, app.js, app.css and
	// img/*.webp (the web package's embed FS). nil serves a plain-text
	// placeholder for each page.
	Pages fs.FS
	// DuplicateOrder is the TEST_MUTATION hook passed to PlaceOrder; nil
	// in normal use.
	DuplicateOrder func(clientRef string) bool
	// DuplicateDone settles DuplicateOrder after the transaction ends.
	DuplicateDone func(clientRef, outcome string)
	// Signals delivers SIGTERM to Run; nil makes Run subscribe to
	// SIGTERM and SIGINT itself.
	Signals <-chan os.Signal
	// Exit ends the process for the step-0 SIGTERM path; nil is os.Exit.
	Exit func(code int)
}

// Server is the HTTP application. Build it with New.
type Server struct {
	cfg      config.Config
	deps     Deps
	log      *slog.Logger
	shutdown chan struct{} // closed by RegisterOnShutdown, read by /events
	started  atomic.Bool   // set once the listener is open
	inflight atomic.Int64  // requests currently inside the middleware
	handler  http.Handler
}

// pingTimeout bounds the readiness database check.
const pingTimeout = 2 * time.Second

// New builds the server and its route table.
func New(cfg config.Config, deps Deps) *Server {
	if deps.Hooks == nil {
		deps.Hooks = hooks.Noop{}
	}
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	if deps.FinalLog == nil {
		deps.FinalLog = deps.Log
	}
	s := &Server{cfg: cfg, deps: deps, log: deps.Log, shutdown: make(chan struct{})}
	s.handler = s.wrap(s.routes())
	return s
}

// Handler is the full handler: middleware around the routes.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	// Pages (Task 11 supplies Deps.Pages). "/{$}" matches only "/", so
	// unknown paths and methods fall through to 404.
	mux.HandleFunc("GET /{$}", s.page("order.html"))
	mux.HandleFunc("GET /kitchen", s.page("kitchen.html"))
	mux.HandleFunc("GET /board", s.page("board.html"))
	mux.HandleFunc("GET /app.js", s.page("app.js"))
	mux.HandleFunc("GET /app.css", s.page("app.css"))
	mux.HandleFunc("GET /img/{file}", s.handleImage)

	mux.HandleFunc("GET /api/menu", s.handleMenu)
	mux.HandleFunc("POST /orders", s.handlePlaceOrder)
	mux.HandleFunc("GET /api/orders", s.handlePending)
	mux.HandleFunc("POST /orders/{id}/ready", s.handleReady)
	if s.cfg.Version == "v2" {
		mux.HandleFunc("POST /api/menu/{id}/available", s.handleAvailable)
	}
	if s.deps.Broadcaster != nil {
		mux.Handle("GET /events", &board.Handler{
			Broadcaster: s.deps.Broadcaster,
			Pool:        s.deps.Pool,
			Cfg:         s.cfg,
			Log:         s.log,
			Shutdown:    s.shutdown,
			Hooks:       s.deps.Hooks,
		})
	}
	mux.HandleFunc("GET /api/audit", s.handleAudit)
	mux.HandleFunc("GET /api/whoami", s.handleWhoami)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

func (s *Server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Pages == nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("koupa " + s.cfg.Version + ": " + name + " placeholder\n"))
			return
		}
		http.ServeFileFS(w, r, s.deps.Pages, name)
	}
}

// imageCache lets browsers keep the drink photos for a day; a new photo
// gets a new file name.
const imageCache = "public, max-age=86400"

// handleImage serves the drink photos (img/*.webp in Deps.Pages). Only
// .webp files that exist are served; everything else is 404.
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if s.deps.Pages == nil || !strings.HasSuffix(name, ".webp") || !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	path := "img/" + name
	if st, err := fs.Stat(s.deps.Pages, path); err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Cache-Control", imageCache)
	http.ServeFileFS(w, r, s.deps.Pages, path)
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"pod_uid": s.cfg.PodUID, "pod_name": s.cfg.PodName, "version": s.cfg.Version,
	})
}

// handleHealthz is readiness (design 4.5, step 1's readinessProbe): the server is listening, the
// database answers, and the broadcaster's LISTEN is active now.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if !s.started.Load() {
		writeError(w, http.StatusServiceUnavailable, "server not started")
		return
	}
	b := s.deps.Broadcaster
	if b == nil {
		writeError(w, http.StatusServiceUnavailable, "no broadcaster")
		return
	}
	select {
	case <-b.Ready():
	default:
		writeError(w, http.StatusServiceUnavailable, "broadcaster not ready")
		return
	}
	if !b.Listening() {
		writeError(w, http.StatusServiceUnavailable, "LISTEN not active")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pingTimeout)
	defer cancel()
	if s.deps.Pool == nil || s.deps.Pool.Ping(ctx) != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
