package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aturzone/chevaletAnonBot/internal/bot"
	"github.com/aturzone/chevaletAnonBot/internal/config"
)

// TestPollLiveness locks the /healthz policy: inside the startup grace the
// process is alive by definition; after it, liveness requires a recent
// successful getUpdates.
func TestPollLiveness(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		started   time.Time
		status    bot.PollStatus
		wantOK    bool
		wantInMsg string
	}{
		{
			name:    "inside startup grace, no poll yet",
			started: now.Add(-time.Minute),
			wantOK:  true,
		},
		{
			name:    "inside startup grace despite an old success",
			started: now.Add(-time.Minute),
			status:  bot.PollStatus{LastSuccess: now.Add(-10 * time.Minute)},
			wantOK:  true,
		},
		{
			name:      "past grace, never polled",
			started:   now.Add(-startupGrace - time.Minute),
			wantOK:    false,
			wantInMsg: "no successful getUpdates",
		},
		{
			name:    "past grace, recent success",
			started: now.Add(-startupGrace - time.Minute),
			status:  bot.PollStatus{LastSuccess: now.Add(-30 * time.Second)},
			wantOK:  true,
		},
		{
			name:      "past grace, stale success",
			started:   now.Add(-startupGrace - time.Minute),
			status:    bot.PollStatus{LastSuccess: now.Add(-pollStaleAfter - time.Second)},
			wantOK:    false,
			wantInMsg: "no successful getUpdates",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := pollLiveness(now, tc.started, tc.status)
			if ok != tc.wantOK {
				t.Fatalf("pollLiveness = (%v, %q); want ok=%v", ok, reason, tc.wantOK)
			}
			if !ok && tc.wantInMsg != "" && !strings.Contains(reason, tc.wantInMsg) {
				t.Errorf("reason %q does not contain %q", reason, tc.wantInMsg)
			}
			if ok && reason != "starting" && reason != "" {
				t.Errorf("healthy liveness reason = %q; want empty", reason)
			}
		})
	}
}

// TestRuntimeStateLiveness: the listener starts before the bot exists, so the
// state wrapper must answer nil-safely and switch to the real poll status later.
func TestRuntimeStateLiveness(t *testing.T) {
	state := newRuntimeState()
	if ok, reason := state.liveness(); !ok || reason != "starting" {
		t.Errorf("booting liveness = (%v, %q); want (true, starting)", ok, reason)
	}

	// Pretend booting took longer than the grace.
	state.start = time.Now().Add(-startupGrace - time.Minute)
	if ok, reason := state.liveness(); ok || !strings.Contains(reason, "not initialised") {
		t.Errorf("stuck-boot liveness = (%v, %q); want not-initialised failure", ok, reason)
	}

	// With a bot (zero-value is enough: no successful poll) and an old start.
	state.setBot(&bot.Bot{})
	if ok, reason := state.liveness(); ok || !strings.Contains(reason, "getUpdates") {
		t.Errorf("never-polled liveness = (%v, %q); want a getUpdates failure", ok, reason)
	}

	// A fresh start is within the grace again.
	state.start = time.Now()
	if ok, reason := state.liveness(); !ok || reason != "starting" {
		t.Errorf("fresh-start liveness = (%v, %q); want (true, starting)", ok, reason)
	}
}

// TestRuntimeStateReadiness: /readyz is 503 until both the assets are validated
// and the database is connected, and the reason says which one is missing.
func TestRuntimeStateReadiness(t *testing.T) {
	ctx := context.Background()
	state := newRuntimeState()

	ok, reason := state.readiness(ctx)
	if ok || !strings.Contains(reason, "assets") {
		t.Errorf("pre-assets readiness = (%v, %q); want an assets failure", ok, reason)
	}

	state.setAssets(errors.New("runtime assets: Texts directory \"/app/Texts\": missing"))
	if ok, reason := state.readiness(ctx); ok || !strings.Contains(reason, "Texts") {
		t.Errorf("assets-failed readiness = (%v, %q); want the underlying reason", ok, reason)
	}

	state.setAssets(nil)
	if ok, reason := state.readiness(ctx); ok || !strings.Contains(reason, "database") {
		t.Errorf("no-DB readiness = (%v, %q); want a database failure", ok, reason)
	}
}

// TestValidateTextsDir: a missing/empty Texts directory fails closed; one
// non-empty .txt template is enough.
func TestValidateTextsDir(t *testing.T) {
	if err := validateTextsDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing Texts dir: want an error")
	}

	dir := t.TempDir()
	// A file where the directory should be.
	filePath := filepath.Join(dir, "Texts")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateTextsDir(filePath); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("validateTextsDir(file) = %v; want a not-a-directory error", err)
	}

	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(empty, "empty.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateTextsDir(empty); err == nil || !strings.Contains(err.Error(), "no non-empty") {
		t.Errorf("validateTextsDir(empty templates) = %v; want a no-non-empty error", err)
	}

	good := t.TempDir()
	if err := os.MkdirAll(filepath.Join(good, "settings"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "start.txt"), []byte("سلام"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateTextsDir(good); err != nil {
		t.Errorf("validateTextsDir(valid) = %v; want nil", err)
	}
}

// TestValidateDynsetPath: the state file's parent must be writable (proven with a
// real temp write), and a path whose parent cannot be a directory must fail.
func TestValidateDynsetPath(t *testing.T) {
	dir := t.TempDir()
	if err := validateDynsetPath(filepath.Join(dir, "state", "dynamic_settings.json")); err != nil {
		t.Errorf("validateDynsetPath(creatable parent) = %v; want nil", err)
	}

	// A regular FILE used as the parent directory: MkdirAll must fail.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateDynsetPath(filepath.Join(blocker, "dynamic_settings.json")); err == nil {
		t.Error("validateDynsetPath(parent is a file) = nil; want an error")
	}

	if err := validateDynsetPath(""); err == nil {
		t.Error("validateDynsetPath(empty) = nil; want an error")
	}
	// The temp write-check file must not be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".dynset-writecheck-") {
			t.Errorf("write-check file %q was not cleaned up", e.Name())
		}
	}
}

// TestValidateBackupsDir: a missing/unreadable backups dir fails the boot; an
// empty one (fresh install) is fine; a dump is logged as readable.
func TestValidateBackupsDir(t *testing.T) {
	if err := validateBackupsDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing backups dir: want an error")
	}

	empty := t.TempDir()
	if err := validateBackupsDir(empty); err != nil {
		t.Errorf("empty backups dir = %v; want nil", err)
	}

	withDump := t.TempDir()
	if err := os.WriteFile(filepath.Join(withDump, "backup_20260101_000001.sql.gz"), []byte("gz"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateBackupsDir(withDump); err != nil {
		t.Errorf("backups dir with a dump = %v; want nil", err)
	}
}

// TestValidateAssetsCombined exercises the boot gate end to end with real
// directories (the production wrapper passes "Texts" and "backups").
func TestValidateAssetsCombined(t *testing.T) {
	textsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(textsDir, "start.txt"), []byte("سلام"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupsDir := t.TempDir()
	cfg := &config.Config{DynsetPath: filepath.Join(t.TempDir(), "state", "dynamic_settings.json")}

	if err := validateAssets(cfg, textsDir, backupsDir); err != nil {
		t.Errorf("validateAssets(valid) = %v; want nil", err)
	}
	if err := validateAssets(cfg, filepath.Join(t.TempDir(), "nope"), backupsDir); err == nil {
		t.Error("validateAssets(missing texts) = nil; want an error")
	}
}

// TestConnectDBRetryBounded: boot retries are bounded in count AND total time, so
// a permanently unreachable database cannot hang startup forever. Port 1 on the
// loopback is closed, so each attempt fails fast.
func TestConnectDBRetryBounded(t *testing.T) {
	cfg := &config.Config{
		DBHost:           "127.0.0.1",
		DBPort:           1,
		DBName:           "nope",
		DBUser:           "nope",
		DBPass:           "nope",
		DBSSLMode:        "disable",
		DBConnectTimeout: 200 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	database, err := connectDBRetry(ctx, cfg, 2, 300*time.Millisecond, 20*time.Millisecond)
	elapsed := time.Since(start)
	if database != nil {
		database.Close()
		t.Fatal("connected to a closed port; the test environment is unusable for this check")
	}
	if err == nil || !strings.Contains(err.Error(), "database unreachable") {
		t.Fatalf("connectDBRetry = %v; want a database-unreachable error", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("connectDBRetry took %s; the retry/budget bounds were not enforced", elapsed)
	}
}

// TestHeartbeatOnce: the optional HEARTBEAT_URL GET fires once per call, tolerates
// error statuses without panicking, and swallows malformed URLs.
func TestHeartbeatOnce(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: time.Second}
	heartbeatOnce(context.Background(), client, srv.URL)
	if got := hits.Load(); got != 1 {
		t.Errorf("heartbeat hit count = %d; want 1", got)
	}

	// A 500 response is logged, not fatal.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer errSrv.Close()
	heartbeatOnce(context.Background(), client, errSrv.URL)

	// A malformed URL must not panic.
	heartbeatOnce(context.Background(), client, "://not-a-url")
}

// TestSleepConstantsSound sanity-checks the probe intervals: the watchdog must
// poll more often than the liveness threshold, and the liveness threshold must
// leave room for a normal 9s long poll.
func TestSleepConstantsSound(t *testing.T) {
	if watchdogInterval >= pollStaleAfter {
		t.Errorf("watchdogInterval %s >= pollStaleAfter %s; a stall could exceed the health threshold between checks",
			watchdogInterval, pollStaleAfter)
	}
	if pollStaleAfter <= 9*time.Second {
		t.Errorf("pollStaleAfter %s leaves no margin over the 9s long poll", pollStaleAfter)
	}
	if dbConnectBudget <= 0 || dbConnectAttempts <= 0 {
		t.Error("database retry budget/attempts must be positive")
	}
}
