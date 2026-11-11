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
	}{{"v1", true}, {"v2", true}, {"v1.2.0", true}, {"v2.2.0", true}, {"v2.10.0", true}, {"v1.0", true},
		{"dev", false}, {"", false}, {"v3", false}, {"v3.0.0", false}, {"v0.1.0", false},
		{"1.2.0", false}, {"v1.2.0.1", false}, {"v1.2.0-rc1", false}, {"v01.2.0", false}} {
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

func TestMajor(t *testing.T) {
	for _, tc := range []struct {
		v     string
		major int
		sold  bool
	}{
		{"v1.2.0", 1, false}, {"v1", 1, false}, {"v1.0.0", 1, false},
		{"v2.2.0", 2, true}, {"v2", 2, true}, {"v2.0", 2, true}, {"v2.10.3", 2, true},
		{"v3.0.0", 3, true}, {"v10.0.0", 10, true}, {"v0.9.0", 0, false},
		{"dev", 0, false}, {"", 0, false}, {"2.2.0", 0, false}, {"v2.x", 0, false},
		{"v2.2.0-rc1", 0, false}, {"v2.2.0.1", 0, false}, {"v02.0.0", 0, false},
		{"v", 0, false}, {"v2.", 0, false}, {" v2.2.0", 0, false}, {"V2.2.0", 0, false},
		{"v99999999999999999999", 0, false},
	} {
		if got := Major(tc.v); got != tc.major {
			t.Errorf("Major(%q) = %d, want %d", tc.v, got, tc.major)
		}
		if got := HasSoldOut(tc.v); got != tc.sold {
			t.Errorf("HasSoldOut(%q) = %v, want %v", tc.v, got, tc.sold)
		}
		if got := (Config{Version: tc.v}).SoldOut(); got != tc.sold {
			t.Errorf("Config{Version: %q}.SoldOut() = %v, want %v", tc.v, got, tc.sold)
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

func TestLoadFillerBytes(t *testing.T) {
	withVersion(t, "v1")
	cfg, err := Load(env(map[string]string{"DATABASE_URL": "u", "TEST_HOOKS": "1", "TEST_SSE_FILLER_BYTES": "65536"}))
	if err != nil || cfg.SSEFillerBytes != 65536 {
		t.Fatalf("got %d, %v", cfg.SSEFillerBytes, err)
	}
	for _, bad := range []string{"-1", "2", "x", "2000000"} {
		if _, err := Load(env(map[string]string{"DATABASE_URL": "u", "TEST_HOOKS": "1", "TEST_SSE_FILLER_BYTES": bad})); err == nil {
			t.Errorf("TEST_SSE_FILLER_BYTES=%q accepted", bad)
		}
	}
}

// Without TEST_HOOKS the filler setting is ignored, even when invalid.
func TestFillerBytesIgnoredWithoutHooks(t *testing.T) {
	withVersion(t, "v1")
	cfg, err := Load(env(map[string]string{"DATABASE_URL": "u", "TEST_SSE_FILLER_BYTES": "x"}))
	if err != nil || cfg.SSEFillerBytes != 0 {
		t.Fatalf("got %d, %v; want 0, nil", cfg.SSEFillerBytes, err)
	}
	cfg, err = Load(env(map[string]string{"DATABASE_URL": "u", "TEST_SSE_FILLER_BYTES": "4096"}))
	if err != nil || cfg.SSEFillerBytes != 0 {
		t.Fatalf("got %d, %v; want 0 without TEST_HOOKS", cfg.SSEFillerBytes, err)
	}
}
