// Command koupa is the tea-shop app: order, kitchen and call-number board.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"koupa/app/applog"
	"koupa/app/broadcast"
	"koupa/app/config"
	"koupa/app/hooks"
	"koupa/app/server"
	"koupa/app/store"
	"koupa/app/web"
)

// startupTimeout bounds the initial database connection.
const startupTimeout = 30 * time.Second

func main() {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "koupa: config:", err)
		os.Exit(1)
	}
	// All logging goes through the gate so the final line (sigterm or
	// exit) really is the last one.
	gate := applog.NewGate(os.Stdout)
	log := applog.New(gate, cfg.PodUID, cfg.PodName, cfg.Version)
	finalLog := applog.New(gate.Final(), cfg.PodUID, cfg.PodName, cfg.Version)

	// Subscribe before anything slow so an early SIGTERM is not lost.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	log.Info(applog.EvStartup,
		"listen_addr", cfg.ListenAddr,
		"graceful_shutdown", cfg.GracefulShutdown,
		"shutdown_timeout", cfg.ShutdownTimeout.String(),
		"resume_from_last_id", cfg.ResumeFromLastID,
		"idempotency_key", cfg.IdempotencyKey,
		"sse_heartbeat", cfg.SSEHeartbeat.String(),
		"sse_retry", cfg.SSERetry.String(),
		"lock_timeout", cfg.LockTimeout.String(),
		"db_timeout", cfg.DBTimeout.String(),
		"broadcast_poll", cfg.BroadcastPoll.String(),
		"sse_write_deadline", cfg.WriteDeadline.String(),
		"test_hooks", cfg.TestHooks,
		"test_mutation_drop_ready", cfg.TestMutation.DropReady,
		"test_mutation_duplicate_order", cfg.TestMutation.DuplicateOrder,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Startup can block on the database; SIGTERM is handled meanwhile.
	sctx, scancel := context.WithTimeout(ctx, startupTimeout)
	openc := make(chan opened, 1)
	go func() {
		p, err := store.NewPool(sctx, cfg, log)
		openc <- opened{p, err}
	}()
	o, sig := awaitStartup(openc, sigs)
	if sig != nil {
		if !cfg.GracefulShutdown {
			finalLog.Info(applog.EvSigterm, "signal", sig.String(), "graceful", false, "phase", "startup")
			os.Exit(0)
		}
		log.Info(applog.EvSigterm, "signal", sig.String(), "graceful", true, "phase", "startup")
		scancel()
		if o.pool != nil {
			o.pool.Close()
		}
		finalLog.Info(applog.EvExit, "clean", true, "phase", "startup")
		os.Exit(0)
	}
	pool, err := o.pool, o.err
	scancel()
	if err != nil {
		log.Error(applog.EvStartupFailed, "err", err.Error())
		fmt.Fprintln(os.Stderr, "koupa: database:", err)
		os.Exit(1)
	}

	hk := hooks.New(cfg.TestHooks, log)
	hk.SetPod(cfg.PodUID, cfg.PodName)
	if cfg.TestHooks {
		// Separate port, never on the main mux.
		go func() {
			if err := hk.Run(ctx, cfg.HooksAddr); err != nil {
				log.Error(applog.EvStartupFailed, "err", "hooks control port: "+err.Error())
			}
		}()
	}
	var drop broadcast.DropFunc
	if f := hooks.DropReady(cfg.TestMutation.DropReady, log); f != nil {
		drop = f
	}
	b := broadcast.New(pool, cfg.DatabaseURL, cfg.BroadcastPoll, log, drop)
	go func() { _ = b.Run(ctx) }()
	go store.RunKeyCleanup(ctx, pool, store.CleanupInterval, log)

	err = server.Run(ctx, cfg, server.Deps{
		Pool:           pool,
		Broadcaster:    b,
		Log:            log,
		FinalLog:       finalLog,
		Hooks:          hk,
		DuplicateOrder: hooks.DuplicateOrder(cfg.TestMutation.DuplicateOrder, log),
		Pages:          web.Pages(),
		Signals:        sigs,
	})
	if err != nil {
		log.Error(applog.EvStartupFailed, "err", err.Error())
		fmt.Fprintln(os.Stderr, "koupa:", err)
		os.Exit(1)
	}
	// server.Run logged exit; leave now. Closing the pool would wait for
	// connections still held by requests Shutdown gave up on.
	os.Exit(0)
}

// opened is the result of opening the pool at startup.
type opened struct {
	pool *pgxpool.Pool
	err  error
}

// awaitStartup waits for the pool result or a signal. A signal that is
// already pending wins even when the result is ready too (select alone
// would pick at random), so SIGTERM during startup is never lost behind a
// failed or finished NewPool. A non-nil signal means: stop now.
func awaitStartup(openc <-chan opened, sigs <-chan os.Signal) (opened, os.Signal) {
	select {
	case sig := <-sigs:
		return opened{}, sig
	case o := <-openc:
		select {
		case sig := <-sigs:
			return o, sig
		default:
			return o, nil
		}
	}
}
