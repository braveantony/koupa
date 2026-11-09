// Package config loads the app's settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Version is set at build time: -ldflags "-X koupa/app/config.Version=v1".
// Load accepts only "v1" or "v2".
var Version = "dev"

// Mutation holds the test-only fault injection counters (TEST_MUTATION).
type Mutation struct {
	DropReady      int
	DuplicateOrder int
}

// Config is the full runtime configuration.
type Config struct {
	DatabaseURL      string
	PodUID           string
	PodName          string
	Version          string
	GracefulShutdown bool
	ShutdownTimeout  time.Duration
	ResumeFromLastID bool
	IdempotencyKey   bool
	SSEHeartbeat     time.Duration // 0 = disabled
	SSERetry         time.Duration
	LockTimeout      time.Duration
	DBTimeout        time.Duration // fixed
	BroadcastPoll    time.Duration
	WriteDeadline    time.Duration
	TestHooks        bool
	TestMutation     Mutation
	ListenAddr       string // LISTEN_ADDR, default ":8080"
}

// Load reads the configuration through getenv (usually os.Getenv).
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		DatabaseURL: getenv("DATABASE_URL"),
		PodUID:      getenv("POD_UID"),
		PodName:     getenv("POD_NAME"),
		Version:     Version,
		DBTimeout:   60 * time.Second,
	}
	if Version != "v1" && Version != "v2" {
		return Config{}, fmt.Errorf("build version %q is not v1 or v2 (set -ldflags -X koupa/app/config.Version=v1)", Version)
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}

	var errs []error
	boolVar := func(dst *bool, key string) {
		switch v := getenv(key); v {
		case "", "0":
			*dst = false
		case "1":
			*dst = true
		default:
			errs = append(errs, fmt.Errorf("%s: %q is not one of 0, 1, empty", key, v))
		}
	}
	durVar := func(dst *time.Duration, key string, def time.Duration) {
		v := getenv(key)
		if v == "" {
			*dst = def
			return
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			errs = append(errs, fmt.Errorf("%s: %q is not a non-negative duration such as 15s", key, v))
			return
		}
		*dst = d
	}

	boolVar(&cfg.GracefulShutdown, "GRACEFUL_SHUTDOWN")
	boolVar(&cfg.ResumeFromLastID, "RESUME_FROM_LAST_ID")
	boolVar(&cfg.IdempotencyKey, "IDEMPOTENCY_KEY")
	boolVar(&cfg.TestHooks, "TEST_HOOKS")
	durVar(&cfg.ShutdownTimeout, "SHUTDOWN_TIMEOUT", 15*time.Second)
	durVar(&cfg.SSEHeartbeat, "SSE_HEARTBEAT", 0)
	durVar(&cfg.SSERetry, "SSE_RETRY", time.Second)
	durVar(&cfg.LockTimeout, "LOCK_TIMEOUT", 5*time.Second)
	durVar(&cfg.BroadcastPoll, "BROADCAST_POLL_INTERVAL", 2*time.Second)
	durVar(&cfg.WriteDeadline, "SSE_WRITE_DEADLINE", 2*time.Second)

	cfg.ListenAddr = getenv("LISTEN_ADDR")
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}

	m, err := parseMutation(getenv("TEST_MUTATION"))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.TestMutation = m

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseMutation(s string) (Mutation, error) {
	var m Mutation
	if strings.TrimSpace(s) == "" {
		return m, nil
	}
	for _, part := range strings.Split(s, ",") {
		key, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return Mutation{}, fmt.Errorf("TEST_MUTATION: %q is not key=n", part)
		}
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 {
			return Mutation{}, fmt.Errorf("TEST_MUTATION: %q is not a non-negative number", val)
		}
		switch key {
		case "drop-ready":
			m.DropReady = n
		case "duplicate-order":
			m.DuplicateOrder = n
		default:
			return Mutation{}, fmt.Errorf("TEST_MUTATION: unknown key %q", key)
		}
	}
	return m, nil
}
