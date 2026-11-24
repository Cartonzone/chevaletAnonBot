package bot

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPollTrackerRecordsUpdates locks the polling tracker: every getUpdates is
// recorded (attempt, then success or error) and the Bot accessors expose it. This
// is what /healthz and the watchdog read — the old TCP probe could not tell a
// polling bot from a wedged one.
func TestPollTrackerRecordsUpdates(t *testing.T) {
	inner := &fakeInner{}
	l := newSendLimiter()
	c := &limitedClient{inner: inner, limiter: l}

	if st := l.poll.status(); !st.Started.IsZero() || !st.LastAttempt.IsZero() {
		t.Fatalf("fresh tracker = %+v; want all zero", st)
	}
	wantStart := time.Now().Add(-time.Minute)
	l.poll.start(wantStart)

	if _, err := c.RequestWithContext(context.Background(), "t", "getUpdates", nil, nil); err != nil {
		t.Fatalf("getUpdates: %v", err)
	}
	st := l.poll.status()
	if !st.Started.Equal(wantStart) {
		t.Errorf("Started = %v; want %v", st.Started, wantStart)
	}
	if st.LastAttempt.IsZero() || st.LastSuccess.IsZero() {
		t.Errorf("after a successful poll: attempt=%v success=%v; both must be set", st.LastAttempt, st.LastSuccess)
	}
	if !st.LastError.IsZero() {
		t.Errorf("LastError = %v after a success; want zero", st.LastError)
	}
	prevSuccess := st.LastSuccess

	// A failing getUpdates records the error (the updater retries; this is
	// diagnostic, not fatal).
	inner.err = errors.New("proxy: connection reset")
	if _, err := c.RequestWithContext(context.Background(), "t", "getUpdates", nil, nil); err == nil {
		t.Fatal("expected the getUpdates error to propagate")
	}
	st = l.poll.status()
	if st.LastError.IsZero() {
		t.Error("LastError is zero after a failed getUpdates")
	}
	if !st.LastSuccess.Equal(prevSuccess) {
		t.Errorf("a failure overwrote LastSuccess (%v -> %v); the liveness signal must not move back",
			prevSuccess, st.LastSuccess)
	}

	// The Bot accessors are the public API.
	b := &Bot{limiter: l}
	if got := b.LastPollOK(); !got.Equal(st.LastSuccess) {
		t.Errorf("Bot.LastPollOK() = %v; want %v", got, st.LastSuccess)
	}
	if got := b.PollStatus(); !got.LastSuccess.Equal(st.LastSuccess) {
		t.Errorf("Bot.PollStatus().LastSuccess = %v; want %v", got.LastSuccess, st.LastSuccess)
	}

	// A zero-value Bot (before Run wires the limiter) must not panic.
	var empty Bot
	if !empty.LastPollOK().IsZero() {
		t.Error("zero-value Bot.LastPollOK() must be the zero time")
	}
	if st := empty.PollStatus(); !st.LastSuccess.IsZero() {
		t.Error("zero-value Bot.PollStatus() must be empty")
	}
}

// TestIncidentThrottle locks the C-1b throttle: at most one incident report per
// window, duplicates counted silently, and the count handed to the next report so
// nothing is hidden.
func TestIncidentThrottle(t *testing.T) {
	b := &Bot{}

	if suppressed, send := b.beginIncident(); !send || suppressed != 0 {
		t.Fatalf("first incident = (suppressed %d, send %v); want (0, true)", suppressed, send)
	}
	if _, send := b.beginIncident(); send {
		t.Fatal("a second incident inside the window was allowed through; the channel would flood")
	}
	if suppressed, send := b.beginIncident(); send || suppressed != 2 {
		t.Fatalf("third incident = (suppressed %d, send %v); want (2, false)", suppressed, send)
	}

	// Once the window has passed the next incident is sent and reports the number
	// suppressed meanwhile, then the counter resets.
	b.incidentMu.Lock()
	b.lastIncidentAt = time.Now().Add(-incidentWindow - time.Second)
	b.incidentMu.Unlock()
	suppressed, send := b.beginIncident()
	if !send || suppressed != 2 {
		t.Fatalf("after the window = (suppressed %d, send %v); want (2, true)", suppressed, send)
	}
	if suppressed, send := b.beginIncident(); send || suppressed != 1 {
		t.Fatalf("after reset = (suppressed %d, send %v); want (1, false)", suppressed, send)
	}
}

// TestDatabaseTimeoutSplit locks the interactive/background DB timeout split: a
// handler's b.bg() must use the short interactive budget (the dispatcher is
// serial), while background jobs keep the long one.
func TestDatabaseTimeoutSplit(t *testing.T) {
	if dbOpTimeoutInteractive >= dbOpTimeoutBackground {
		t.Errorf("interactive timeout %s is not shorter than background %s",
			dbOpTimeoutInteractive, dbOpTimeoutBackground)
	}
	if dbOpTimeout != dbOpTimeoutBackground {
		t.Errorf("dbOpTimeout alias = %s; want the background budget for legacy call sites", dbOpTimeout)
	}
	if dbOpTimeoutInteractive != 5*time.Second {
		t.Errorf("dbOpTimeoutInteractive = %s; want 5s", dbOpTimeoutInteractive)
	}
	if dbOpTimeoutBackground != 30*time.Second {
		t.Errorf("dbOpTimeoutBackground = %s; want 30s", dbOpTimeoutBackground)
	}

	b := &Bot{}
	ctx, cancel := b.bg()
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("b.bg() has no deadline; a wedged DB query could stall the serial dispatcher forever")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > dbOpTimeoutInteractive {
		t.Errorf("b.bg() deadline is %s away; want within the interactive budget (%s)",
			remaining, dbOpTimeoutInteractive)
	}
}

// TestTelegramClientTimeouts locks H-6: with or without PROXY the default request
// budget stays 10s (the old proxy path used 30s, stalling the serial dispatcher)
// and the transport carries real dial/TLS/response-header timeouts.
func TestTelegramClientTimeouts(t *testing.T) {
	if telegramRequestTimeout != 10*time.Second {
		t.Fatalf("telegramRequestTimeout = %s; want 10s even with PROXY set", telegramRequestTimeout)
	}
	proxy, err := url.Parse("http://127.0.0.1:3128")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		proxy *url.URL
	}{
		{"no proxy", nil},
		{"with proxy", proxy},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			client := newTelegramBotClient(tc.proxy)
			if client.DefaultRequestOpts == nil || client.DefaultRequestOpts.Timeout != telegramRequestTimeout {
				t.Errorf("DefaultRequestOpts.Timeout = %v; want %s", client.DefaultRequestOpts, telegramRequestTimeout)
			}
			transport, ok := client.Client.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport is %T; want *http.Transport", client.Client.Transport)
			}
			if transport.DialContext == nil {
				t.Error("transport has no dial timeout")
			}
			if transport.TLSHandshakeTimeout != telegramTLSHandshakeTimeout {
				t.Errorf("TLSHandshakeTimeout = %s; want %s", transport.TLSHandshakeTimeout, telegramTLSHandshakeTimeout)
			}
			if transport.ResponseHeaderTimeout != telegramResponseHeaderTimeout {
				t.Errorf("ResponseHeaderTimeout = %s; want %s", transport.ResponseHeaderTimeout, telegramResponseHeaderTimeout)
			}
			if transport.Proxy == nil {
				t.Fatal("transport has no proxy resolver; a PROXY configuration would be ignored")
			}
			got, perr := transport.Proxy(&http.Request{})
			if perr != nil {
				t.Fatalf("proxy resolver: %v", perr)
			}
			if tc.proxy == nil && got != nil {
				t.Errorf("proxy resolver returned %v without PROXY; want nil", got)
			}
			if tc.proxy != nil && (got == nil || got.String() != tc.proxy.String()) {
				t.Errorf("proxy resolver = %v; want %v", got, tc.proxy)
			}
		})
	}
}

// TestBackupFreshnessRules locks the backup-loop safety rules: the nightly path
// refuses a stale (>2h) dump instead of shipping it, and /admin backup refuses a
// file younger than 60s (a dump still being written).
func TestBackupFreshnessRules(t *testing.T) {
	if backupStaleAfter != 2*time.Hour {
		t.Errorf("backupStaleAfter = %s; want 2h", backupStaleAfter)
	}
	if backupMinAge != 3*time.Minute {
		t.Errorf("backupMinAge = %s; want 3m", backupMinAge)
	}
	if adminBackupMinAge != time.Minute {
		t.Errorf("adminBackupMinAge = %s; want 60s", adminBackupMinAge)
	}

	dir := t.TempDir()
	write := func(name string, age time.Duration) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	write("backup_20260101_000001.sql.gz", 3*time.Hour) // stale
	write("backup_20260101_030001.sql.gz", 0)           // just written (in flight)

	// backupFileAge reports the age used by the staleness guard.
	age, err := backupFileAge(dir, "backup_20260101_000001.sql.gz")
	if err != nil {
		t.Fatalf("backupFileAge: %v", err)
	}
	if age < backupStaleAfter {
		t.Errorf("3h-old backup reported as %s; the staleness guard would not fire", age)
	}
	if _, err := backupFileAge(dir, "missing.sql.gz"); err == nil {
		t.Error("backupFileAge(missing) returned nil error")
	}

	// /admin backup's 60s guard must skip the just-written file...
	latest, err := latestBackupFile(dir, adminBackupMinAge)
	if err != nil {
		t.Fatal(err)
	}
	if latest != "backup_20260101_000001.sql.gz" {
		t.Errorf("latestBackupFile(adminBackupMinAge) = %q; want the settled file", latest)
	}
	// ...while the nightly 3min guard skips both a 0s-old and a 1min-old dump.
	write("backup_20260101_030501.sql.gz", time.Minute)
	latest, err = latestBackupFile(dir, backupMinAge)
	if err != nil {
		t.Fatal(err)
	}
	if latest != "backup_20260101_000001.sql.gz" {
		t.Errorf("latestBackupFile(backupMinAge) = %q; want the settled file", latest)
	}
}
