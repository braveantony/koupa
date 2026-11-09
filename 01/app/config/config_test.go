package config

import (
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// withVersion sets config.Version for one test and restores it afterwards.
func withVersion(t *testing.T, v string) {
	t.Helper()
	old := Version
	Version = v
	t.Cleanup(func() { Version = old })
}

func TestLoadVersion(t *testing.T) {
	for _, tc := range []struct {
		v  string
		ok bool
	}{{"v1", true}, {"v2", true}, {"dev", false}, {"", false}, {"v3", false}} {
		withVersion(t, tc.v)
		cfg, err := Load(env(map[string]string{"DATABASE_URL": "u"}))
		if tc.ok && (err != nil || cfg.Version != tc.v) {
			t.Errorf("Version=%q: got %q, %v", tc.v, cfg.Version, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("Version=%q: want error", tc.v)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	withVersion(t, "v1")
	cfg, err := Load(env(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "postgres://x" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.GracefulShutdown || cfg.ResumeFromLastID || cfg.IdempotencyKey || cfg.TestHooks {
		t.Errorf("bools should default to false: %+v", cfg)
	}
	checks := []struct {
		name      string
		got, want time.Duration
	}{
		{"ShutdownTimeout", cfg.ShutdownTimeout, 15 * time.Second},
		{"SSEHeartbeat", cfg.SSEHeartbeat, 0},
		{"SSERetry", cfg.SSERetry, time.Second},
		{"LockTimeout", cfg.LockTimeout, 5 * time.Second},
		{"DBTimeout", cfg.DBTimeout, 60 * time.Second},
		{"BroadcastPoll", cfg.BroadcastPoll, 2 * time.Second},
		{"WriteDeadline", cfg.WriteDeadline, 2 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if cfg.TestMutation != (Mutation{}) {
		t.Errorf("TestMutation = %+v", cfg.TestMutation)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", cfg.ListenAddr)
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	withVersion(t, "v1")
	if _, err := Load(env(nil)); err == nil {
		t.Fatal("want error without DATABASE_URL")
	}
}

func TestLoadValues(t *testing.T) {
	withVersion(t, "v2")
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL": "u", "POD_UID": "uid-1", "POD_NAME": "p-1",
		"GRACEFUL_SHUTDOWN": "1", "RESUME_FROM_LAST_ID": "1", "IDEMPOTENCY_KEY": "0",
		"TEST_HOOKS": "1", "SHUTDOWN_TIMEOUT": "20s", "SSE_HEARTBEAT": "10s",
		"SSE_RETRY": "3s", "LOCK_TIMEOUT": "7s", "BROADCAST_POLL_INTERVAL": "500ms",
		"SSE_WRITE_DEADLINE": "4s", "LISTEN_ADDR": "127.0.0.1:9999",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:9999" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.PodUID != "uid-1" || cfg.PodName != "p-1" || cfg.Version != "v2" {
		t.Errorf("identity wrong: %+v", cfg)
	}
	if !cfg.GracefulShutdown || !cfg.ResumeFromLastID || cfg.IdempotencyKey || !cfg.TestHooks {
		t.Errorf("bools wrong: %+v", cfg)
	}
	if cfg.ShutdownTimeout != 20*time.Second || cfg.SSEHeartbeat != 10*time.Second ||
		cfg.SSERetry != 3*time.Second || cfg.LockTimeout != 7*time.Second ||
		cfg.BroadcastPoll != 500*time.Millisecond || cfg.WriteDeadline != 4*time.Second {
		t.Errorf("durations wrong: %+v", cfg)
	}
}

func TestLoadHeartbeatZero(t *testing.T) {
	withVersion(t, "v1")
	cfg, err := Load(env(map[string]string{"DATABASE_URL": "u", "SSE_HEARTBEAT": "0"}))
	if err != nil || cfg.SSEHeartbeat != 0 {
		t.Fatalf("got %v, %v", cfg.SSEHeartbeat, err)
	}
}

func TestLoadRejectsBadBool(t *testing.T) {
	withVersion(t, "v1")
	for _, v := range []string{"yes", "true", "2"} {
		_, err := Load(env(map[string]string{"DATABASE_URL": "u", "GRACEFUL_SHUTDOWN": v}))
		if err == nil {
			t.Errorf("GRACEFUL_SHUTDOWN=%q: want error", v)
		}
	}
}

func TestLoadRejectsBadDuration(t *testing.T) {
	withVersion(t, "v1")
	for _, v := range []string{"abc", "-1s", "5"} {
		_, err := Load(env(map[string]string{"DATABASE_URL": "u", "LOCK_TIMEOUT": v}))
		if err == nil {
			t.Errorf("LOCK_TIMEOUT=%q: want error", v)
		}
	}
}

func TestLoadMutation(t *testing.T) {
	withVersion(t, "v1")
	cfg, err := Load(env(map[string]string{"DATABASE_URL": "u", "TEST_MUTATION": "drop-ready=3"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TestMutation.DropReady != 3 || cfg.TestMutation.DuplicateOrder != 0 {
		t.Errorf("got %+v", cfg.TestMutation)
	}
	cfg, err = Load(env(map[string]string{"DATABASE_URL": "u", "TEST_MUTATION": "drop-ready=3, duplicate-order=5"}))
	if err != nil || cfg.TestMutation != (Mutation{DropReady: 3, DuplicateOrder: 5}) {
		t.Errorf("got %+v, %v", cfg.TestMutation, err)
	}
}

func TestLoadMutationRejects(t *testing.T) {
	withVersion(t, "v1")
	for _, v := range []string{"bogus=1", "drop-ready=x", "drop-ready=-1", "drop-ready", "drop-ready=1,,"} {
		_, err := Load(env(map[string]string{"DATABASE_URL": "u", "TEST_MUTATION": v}))
		if err == nil {
			t.Errorf("TEST_MUTATION=%q: want error", v)
		}
	}
}

func TestLoadHooksAddr(t *testing.T) {
	withVersion(t, "v1")
	cfg, err := Load(env(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil || cfg.HooksAddr != ":9090" {
		t.Fatalf("default = %q, %v", cfg.HooksAddr, err)
	}
	cfg, err = Load(env(map[string]string{"DATABASE_URL": "postgres://x", "HOOKS_ADDR": "127.0.0.1:1"}))
	if err != nil || cfg.HooksAddr != "127.0.0.1:1" {
		t.Fatalf("set = %q, %v", cfg.HooksAddr, err)
	}
}
