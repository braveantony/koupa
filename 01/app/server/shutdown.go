package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"koupa/app/applog"
	"koupa/app/config"
)

// readHeaderTimeout bounds how long a client may take to send headers.
const readHeaderTimeout = 10 * time.Second

// Run listens on cfg.ListenAddr and serves until SIGTERM (or SIGINT, or
// ctx ending, which is treated as a graceful stop).
//
// GracefulShutdown=false (step 0, design 4.4): log sigterm and exit at once
// through Deps.Exit; nothing else runs.
//
// GracefulShutdown=true (step 3, plan A5): log sigterm, then call
// http.Server.Shutdown immediately with ShutdownTimeout. Shutdown closes the
// listener first; the function registered with RegisterOnShutdown only
// closes the shared channel every /events handler watches. If Shutdown
// times out, log the number of unfinished requests. Log exit last and
// return; the caller exits right away.
func Run(ctx context.Context, cfg config.Config, deps Deps) error {
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.ListenAddr, err)
	}
	return New(cfg, deps).Serve(ctx, ln)
}

// Serve is Run on an open listener.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	sigs := s.deps.Signals
	if sigs == nil {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		defer signal.Stop(ch)
		sigs = ch
	}
	exit := s.deps.Exit
	if exit == nil {
		exit = os.Exit
	}

	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	// Only the broadcast: never wait or write here (A5 step 2).
	srv.RegisterOnShutdown(func() { close(s.shutdown) })

	serveErr := make(chan error, 1)
	s.started.Store(true)
	go func() { serveErr <- srv.Serve(ln) }()

	var sig string
	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case got := <-sigs:
		sig = got.String()
	case <-ctx.Done():
		sig = "context done"
	}
	sigArgs := []any{"signal", sig, "graceful", s.cfg.GracefulShutdown, "inflight", s.inflight.Load()}
	if !s.cfg.GracefulShutdown {
		// The only thing step 0 does: this line, then exit.
		s.deps.FinalLog.Info(applog.EvSigterm, sigArgs...)
		exit(0)
		return nil // only reached when Exit is a test double
	}
	s.log.Info(applog.EvSigterm, sigArgs...)

	sctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	err := srv.Shutdown(sctx)
	clean := err == nil
	if errors.Is(err, context.DeadlineExceeded) {
		s.log.Warn(applog.EvShutdownTimeout, "timeout", s.cfg.ShutdownTimeout.String(), "inflight", s.inflight.Load())
	} else if err != nil {
		s.log.Error(applog.EvShutdownTimeout, "err", err.Error(), "inflight", s.inflight.Load())
	}
	s.deps.FinalLog.Info(applog.EvExit, "clean", clean)
	return nil
}
