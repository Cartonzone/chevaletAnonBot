package bot

// Adversarial verification tests for the v3.4.1 runtime/error-path hardening
// (task-7). Separate file: the existing hardening tests cover classification and
// the pre-fail gate; these fill the remaining gaps:
//
//   - the incident throttle must actually FOLD the suppressed count into the one
//     Telegram summary that IS sent (the existing unit test stops at beginIncident);
//   - shutdown drains must be BOUNDED (a hung goroutine cannot hold SIGTERM);
//   - a real database outage -> fast-fail -> recovery cycle (opt-in fault injection
//     against a disposable container; skipped unless VERIFY_FAULT_PG is set).

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/aturzone/chevaletAnonBot/internal/config"
	"github.com/aturzone/chevaletAnonBot/internal/db"
)

// TestVerificationIncidentThrottleChannelMerge drives reportIncident end to end
// against the fake Bot API: three incidents inside the window produce exactly ONE
// channel summary and ONE user tracking-code reply, and the next incident after the
// window carries "(+2 similar suppressed)" so nothing is silently lost.
func TestVerificationIncidentThrottleChannelMerge(t *testing.T) {
	tgBot, fake := newHardeningFakeAPI(t)
	b := &Bot{
		TG:         tgBot,
		Cfg:        &config.Config{ErrorChatID: "-100123456"},
		errReports: newErrReportStore(50),
		limiter:    newSendLimiter(),
	}
	_, msgCtx := hardeningTestMessage(777)

	channelTexts := func(calls []apiCall) []string {
		var out []string
		for _, c := range calls {
			if c.method == "sendMessage" && c.params["chat_id"] == "-100123456" {
				out = append(out, c.params["text"])
			}
		}
		return out
	}

	mark := fake.mark()
	b.reportIncident(tgBot, msgCtx, "AAAAAAAA", "incident one", []string{"page-1"})
	b.reportIncident(tgBot, msgCtx, "BBBBBBBB", "incident two", []string{"page-2"})
	b.reportIncident(tgBot, msgCtx, "CCCCCCCC", "incident three", []string{"page-3"})

	channels := channelTexts(fake.since(mark))
	if len(channels) != 1 {
		t.Fatalf("channel reports = %d (%v); want exactly 1 per incidentWindow", len(channels), channels)
	}
	if !strings.Contains(channels[0], "incident one") {
		t.Errorf("the sent report = %q; want the FIRST incident's summary", channels[0])
	}
	var userReplies int
	for _, c := range fake.since(mark) {
		if c.method == "sendMessage" && c.params["chat_id"] == "777" && strings.Contains(c.params["text"], "کد پیگیری") {
			userReplies++
		}
	}
	if userReplies != 1 {
		t.Errorf("user tracking-code replies = %d; want 1 (throttled incidents must not spam the user)", userReplies)
	}

	// Force the window to expire without sleeping out 30 s.
	b.incidentMu.Lock()
	b.lastIncidentAt = time.Now().Add(-incidentWindow - time.Second)
	b.incidentMu.Unlock()

	mark = fake.mark()
	b.reportIncident(tgBot, msgCtx, "DDDDDDDD", "incident four", []string{"page-4"})
	channels = channelTexts(fake.since(mark))
	if len(channels) != 1 {
		t.Fatalf("post-window channel reports = %d; want 1", len(channels))
	}
	if !strings.Contains(channels[0], "(+2 similar suppressed)") {
		t.Errorf("post-window report = %q; want it to carry \"(+2 similar suppressed)\"", channels[0])
	}
}

// TestVerificationPgErrorUnavailableMatrix pins the widened DB-unavailable
// classification: a PostgreSQL restart surfaces as a server PgError in SQLSTATE
// class 08 (connection exception) or 57P01/57P02/57P03 (admin shutdown / crash
// shutdown / cannot connect now). Those MUST trip the fast-fail gate even though
// they are PgErrors (the server answered before dying); ordinary SQL errors MUST
// NOT, because the database is demonstrably working.
func TestVerificationPgErrorUnavailableMatrix(t *testing.T) {
	unavailable := []string{"08000", "08003", "08006", "57P01", "57P02", "57P03"}
	for _, code := range unavailable {
		err := &pgconn.PgError{Code: code, Message: "server is going away"}
		if !isDBError(err) {
			t.Errorf("SQLSTATE %s: isDBError=false; want true (the DB path, not onError)", code)
		}
		if !isDBUnavailable(err) {
			t.Errorf("SQLSTATE %s: isDBUnavailable=false; fast-fail would be delayed until after a PG restart", code)
		}
	}

	serverSide := []string{"23505", "42601", "40001", "23P01"}
	for _, code := range serverSide {
		err := &pgconn.PgError{Code: code, Message: "ordinary SQL error"}
		if !isDBError(err) {
			t.Errorf("SQLSTATE %s: isDBError=false; a server SQL error belongs on the DB path", code)
		}
		if isDBUnavailable(err) {
			t.Errorf("SQLSTATE %s: isDBUnavailable=true; ordinary SQL errors must NOT trip the fast-fail gate", code)
		}
	}

	// The user-facing error-channel summary stays redacted for the shutdown class.
	if got := errSummary(&pgconn.PgError{Code: "57P01"}); got != "SQLSTATE 57P01" {
		t.Errorf("errSummary(57P01) = %q; want the SQLSTATE-only redacted category", got)
	}
}

// TestVerificationWaitBackgroundBounded proves the shutdown drain cannot hang on a
// wedged background goroutine: waitBackground must return when its budget elapses.
func TestVerificationWaitBackgroundBounded(t *testing.T) {
	b := &Bot{}
	done := make(chan struct{})
	b.bgWG.Add(1)
	go func() {
		defer b.bgWG.Done()
		defer close(done)
		time.Sleep(2 * time.Second) // deliberately outlives the drain budget
	}()

	start := time.Now()
	b.waitBackground(80 * time.Millisecond)
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Errorf("waitBackground took %s with an 80ms budget; shutdown is not bounded", elapsed)
	}
	// Let the goroutine finish so the test does not leak it into other tests.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("background goroutine did not finish")
	}
}

// TestVerificationDatabaseOutageFaultInjection is opt-in fault injection against a
// DISPOSABLE local PostgreSQL container (never production):
//
//	VERIFY_FAULT_PG=verify-pg VERIFY_FAULT_PG_PORT=55433 \
//	  go test ./internal/bot -run TestVerificationDatabaseOutageFaultInjection -v
//
// It connects, stops the container, proves the resulting error is classified as
// DB-unavailable and trips the fast-fail gate, restarts the container and proves a
// successful ping clears the degraded state — the degraded -> fast-fail -> recovery
// cycle the incident fix depends on, with a REAL outage rather than a synthetic
// error value.
func TestVerificationDatabaseOutageFaultInjection(t *testing.T) {
	container := os.Getenv("VERIFY_FAULT_PG")
	port := os.Getenv("VERIFY_FAULT_PG_PORT")
	if container == "" || port == "" {
		t.Skip("VERIFY_FAULT_PG/VERIFY_FAULT_PG_PORT not set; skipping destructive fault injection")
	}
	portNum, err := parsePort(port)
	if err != nil {
		t.Fatalf("VERIFY_FAULT_PG_PORT: %v", err)
	}
	cfg := &config.Config{
		DBHost:           "127.0.0.1",
		DBPort:           portNum,
		DBName:           "chevalet_verify",
		DBUser:           "root",
		DBPass:           "testpass",
		DBSSLMode:        "disable",
		DBConnectTimeout: 2 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	database, err := db.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to the disposable DB: %v", err)
	}
	t.Cleanup(database.Close)
	if err := database.Ping(ctx); err != nil {
		t.Fatalf("initial ping: %v", err)
	}

	docker := func(args ...string) {
		t.Helper()
		cmd := exec.Command("docker", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
	}
	docker("stop", container)

	// The next operation against a stopped server must at least be classified as a
	// DATABASE error (the DB path, not onError — the incident's C-1). Note: the very
	// first error is often a server PgError 57P01 ("terminating connection due to
	// administrator command"), which is deliberately NOT treated as "unavailable";
	// once the pool has to dial again the error becomes a ConnectError/timeout. Keep
	// pinging until that unavailable class appears (bounded).
	var pingErr error
	outageDeadline := time.Now().Add(15 * time.Second)
	for {
		pingCtx, pingCancel := context.WithTimeout(ctx, 3*time.Second)
		pingErr = database.Ping(pingCtx)
		pingCancel()
		if pingErr == nil {
			if time.Now().After(outageDeadline) {
				t.Fatal("ping kept succeeding against a stopped container; fault injection did not take effect")
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if !isDBError(pingErr) {
			t.Fatalf("stopped-DB error %T (%v) is not classified as a DB error at all", pingErr, pingErr)
		}
		if isDBUnavailable(pingErr) {
			break
		}
		if time.Now().After(outageDeadline) {
			t.Fatalf("stopped-DB error never became DB-unavailable: last %T (%v)", pingErr, pingErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
	database.MarkFailure()
	if !database.DegradedFor(time.Minute) {
		t.Error("a real outage did not trip the fast-fail gate")
	}

	docker("start", container)
	// Wait for the server to accept connections again, then prove recovery.
	recoveryDeadline := time.Now().Add(30 * time.Second)
	for {
		pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
		err = database.Ping(pctx)
		pcancel()
		if err == nil {
			break
		}
		if time.Now().After(recoveryDeadline) {
			t.Fatalf("database did not recover after restart: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	database.MarkSuccess()
	if database.DegradedFor(time.Hour) {
		t.Error("a successful ping/MarkSuccess did not clear the degraded state")
	}
}

// parsePort is a tiny local helper so this file does not import strconv for one call.
func parsePort(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, &portError{s}
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

type portError struct{ s string }

func (e *portError) Error() string { return "not a decimal port: " + e.s }
