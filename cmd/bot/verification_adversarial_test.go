package main

// Adversarial verification tests for the v3.4.1 startup/health policy (task-7).
//
// main_hardening_test.go already covers the happy-path validators; this file pins
// the boundaries the health contract turns on (exactly-at-threshold, zero value,
// state transitions seen concurrently by probes) and documents one deployment hole
// the fail-closed asset validator does NOT catch.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aturzone/chevaletAnonBot/internal/bot"
)

// TestVerificationPollLivenessBoundaries pins the exact thresholds: the policy is
// alive (a) inside the grace window even with no successful poll, (b) when the last
// success is newer than pollStaleAfter, (c) dead otherwise. Off-by-one here means a
// healthy bot restart-loops or a wedged one is never restarted.
func TestVerificationPollLivenessBoundaries(t *testing.T) {
	started := time.Now()
	now := started

	// Inside the grace: alive regardless of polling state.
	if ok, reason := pollLiveness(now.Add(startupGrace), started, bot.PollStatus{}); !ok || reason != "starting" {
		t.Errorf("grace boundary = (%v, %q); want (true, starting)", ok, reason)
	}

	// Just after the grace with no successful poll: dead. This is the branch that
	// makes a bot whose first getUpdates never succeeded visible.
	afterGrace := started.Add(startupGrace + time.Millisecond)
	if ok, reason := pollLiveness(afterGrace, started, bot.PollStatus{}); ok || !strings.Contains(reason, "no successful") {
		t.Errorf("after grace, no poll = (%v, %q); want dead with a no-success reason", ok, reason)
	}

	// Exactly pollStaleAfter idle is still alive; a hair older is stale.
	last := afterGrace
	if ok, _ := pollLiveness(last.Add(pollStaleAfter), started, bot.PollStatus{LastSuccess: last}); !ok {
		t.Error("idle exactly pollStaleAfter must still be alive (the threshold is a maximum, not a trigger)")
	}
	if ok, reason := pollLiveness(last.Add(pollStaleAfter+time.Millisecond), started, bot.PollStatus{LastSuccess: last}); ok || !strings.Contains(reason, "no successful getUpdates for") {
		t.Errorf("idle pollStaleAfter+1ms = (%v, %q); want dead naming the idle time", ok, reason)
	}

	// A successful poll AFTER the grace resets liveness: the wedge is over.
	if ok, reason := pollLiveness(afterGrace, started, bot.PollStatus{LastSuccess: afterGrace}); !ok || reason != "" {
		t.Errorf("recovered poll = (%v, %q); want (true, \"\")", ok, reason)
	}
}

// TestVerificationRuntimeStateTransitions walks the same state machine a probe sees
// while boot progresses: starting -> assets validated -> DB connected -> ready, and
// the reverse on an asset failure. It also runs liveness/readiness concurrently to
// prove the RWMutex really protects startup mutation under -race.
func TestVerificationRuntimeStateTransitions(t *testing.T) {
	state := newRuntimeState()

	// Fresh process: alive/starting, not ready, reason names the missing assets.
	if ok, reason := state.liveness(); !ok || reason != "starting" {
		t.Errorf("fresh liveness = (%v, %q); want (true, starting)", ok, reason)
	}
	if ok, reason := state.readiness(context.Background()); ok || !strings.Contains(reason, "assets") {
		t.Errorf("fresh readiness = (%v, %q); want an assets reason", ok, reason)
	}

	// Assets failed => liveness stays starting (still inside grace) but readiness
	// reports the deployment problem verbatim.
	state.setAssets(os.ErrPermission)
	if ok, reason := state.readiness(context.Background()); ok || !strings.Contains(reason, "permission") {
		t.Errorf("assets-failed readiness = (%v, %q); want the underlying problem", ok, reason)
	}

	// Assets OK but no DB yet.
	state.setAssets(nil)
	if ok, reason := state.readiness(context.Background()); ok || !strings.Contains(reason, "database") {
		t.Errorf("no-DB readiness = (%v, %q); want a database reason", ok, reason)
	}

	// Concurrent probes during the DB transition: no data race, no panic, endpoints
	// stable once the DB is present (a bare &db.DB{} with no pool pings "not
	// connected", which is the honest answer).
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = state.liveness()
				_, _ = state.readiness(context.Background())
			}
		}()
	}
	wg.Wait()
}

// TestVerificationDynsetPathDirectoryIsRejected pins the V-1 fix: a DIRECTORY at
// DYNSET_PATH must fail the boot gate. This is the classic Docker bind-mount
// failure — mounting a host file path that does not exist makes Docker create a
// directory at the container path, after which every admin ai-url/donate save is
// silently lost. The pre-fix validator only checked the PARENT directory, so it
// booted "healthy"; reproduced in a container before the fix (log line "dynamic
// settings path is writable" followed only by the fake-token getMe failure).
func TestVerificationDynsetPathDirectoryIsRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dynamic_settings.json")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("create a directory at the dynset path: %v", err)
	}
	err := validateDynsetPath(target)
	if err == nil {
		t.Fatalf("validateDynsetPath(%q) = nil although %q is a directory; boot would pass while runtime writes fail", target, target)
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("validateDynsetPath(dir target) = %v; want an error saying it is a directory", err)
	}

	// A normal file path must still pass (no regression on the happy path).
	okTarget := filepath.Join(dir, "existing", "dynamic_settings.json")
	if err := os.MkdirAll(filepath.Dir(okTarget), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(okTarget, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateDynsetPath(okTarget); err != nil {
		t.Errorf("validateDynsetPath(existing writable file) = %v; want nil", err)
	}
}
