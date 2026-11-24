package bot

// Hardening tests for the v3.4.1 error path (C-1 + M-8).
//
// The 2026-11-23 incident produced two error classes that the bot did not
// classify as database problems:
//
//   - pgx's internal *errTimeout ("timeout: context deadline exceeded") from an
//     established connection whose I/O hit the caller's deadline, and a bare
//     context.DeadlineExceeded from pgxpool.Acquire. Both fell through
//     handleErr's default branch into onError, which files ONE UNTHROTTLED
//     incident and replies a tracking code per failing update — the flood.
//   - *pgconn.ConnectError (Docker DNS unable to resolve the DB hostname), which
//     WAS reported on the throttled DB path but pasted the raw pgx message —
//     including user/database/host — into ERROR_CHAT_ID (M-8).
//
// These tests pin the classification and the routing, without a live database
// where possible (db.DB's fast-fail state is a plain atomic field, so a
// zero-value &db.DB{} behaves exactly like a healthy one).
//
// New file only: the pre-existing tgerr_test.go/errreport_test.go are untouched.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/aturzone/chevaletAnonBot/internal/config"
	"github.com/aturzone/chevaletAnonBot/internal/db"
)

// newHardeningFakeAPI returns a real *gotgbot.Bot whose Bot API calls are answered
// by anchor_integration_test.go's fakeTG, plus that fake so a test can inspect the
// calls. No database is involved.
func newHardeningFakeAPI(t *testing.T) (*gotgbot.Bot, *fakeTG) {
	t.Helper()
	fake := newFakeTG()
	t.Cleanup(fake.srv.Close)

	tg, err := gotgbot.NewBot("123456:TESTTOKEN", &gotgbot.BotOpts{
		BotClient: &gotgbot.BaseBotClient{
			DefaultRequestOpts: &gotgbot.RequestOpts{Timeout: 5 * time.Second, APIURL: fake.srv.URL},
		},
	})
	if err != nil {
		t.Fatalf("build bot against the fake API: %v", err)
	}
	return tg, fake
}

// hardeningTestMessage builds a private-chat message and the matching context, the
// way the dispatcher would populate one.
func hardeningTestMessage(uid int64) (*gotgbot.Message, *ext.Context) {
	msg := &gotgbot.Message{
		MessageId: 7,
		From:      &gotgbot.User{Id: uid, IsBot: false, FirstName: "u"},
		Chat:      gotgbot.Chat{Id: uid, Type: "private"},
		Date:      time.Now().Unix(),
		Text:      "hello",
	}
	return msg, &ext.Context{
		Update:           &gotgbot.Update{Message: msg},
		EffectiveMessage: msg,
		EffectiveChat:    &msg.Chat,
		EffectiveUser:    msg.From,
	}
}

// TestHardeningDBClassification locks the predicate matrix the whole error path
// branches on.
func TestHardeningDBClassification(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}
	deadline := fmt.Errorf("insert cid: %w", context.DeadlineExceeded)
	// A Telegram (net/http) timeout also wraps context.DeadlineExceeded: it must NOT
	// be mistaken for a DB timeout, or the user would be told the database is broken
	// and the incident would be coerced onto the DB path.
	tgTimeout := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/botXXX/sendMessage",
		Err: context.DeadlineExceeded,
	}
	canceled := fmt.Errorf("acquire: %w", context.Canceled)

	cases := []struct {
		name        string
		err         error
		dbError     bool
		unavailable bool
	}{
		{"pg-error 23505", pgErr, true, false},
		{"wrapped pg-error", fmt.Errorf("insert: %w", pgErr), true, false},
		{"wrapped deadline (pool acquire / query timeout)", deadline, true, true},
		{"context canceled (shutdown)", canceled, false, false},
		{"telegram http timeout", tgTimeout, false, false},
		{"plain error", errors.New("boom"), false, false},
		{"nil", nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDBError(tc.err); got != tc.dbError {
				t.Errorf("isDBError = %v; want %v", got, tc.dbError)
			}
			if got := isDBUnavailable(tc.err); got != tc.unavailable {
				t.Errorf("isDBUnavailable = %v; want %v", got, tc.unavailable)
			}
			if got := isContextCanceled(tc.err); got != (tc.name == "context canceled (shutdown)") {
				t.Errorf("isContextCanceled = %v", got)
			}
			// The DB predicates must never swallow a Telegram transport error: that is
			// the "internet problem" reply, not the "database problem" one.
			if tc.name == "telegram http timeout" && !isNetworkError(tc.err) {
				t.Error("the test setup is wrong: a url.Error must be a network error")
			}
		})
	}
}

// TestHardeningConnectErrorClassification builds a REAL *pgconn.ConnectError by
// dialing a closed loopback port: ParseConfig succeeds and only the dial fails,
// which is exactly the shape of the incident's Docker-DNS failure.
func TestHardeningConnectErrorClassification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := pgconn.Connect(ctx, "host=127.0.0.1 port=1 connect_timeout=1")
	if err == nil {
		t.Fatal("connecting to 127.0.0.1:1 unexpectedly succeeded")
	}
	var connErr *pgconn.ConnectError
	if !errors.As(err, &connErr) {
		t.Fatalf("want a *pgconn.ConnectError, got %T: %v", err, err)
	}
	if !isDBError(err) || !isDBUnavailable(err) {
		t.Errorf("a ConnectError must be classified as an unavailable DB (dbError=%v unavailable=%v)",
			isDBError(err), isDBUnavailable(err))
	}
	if got := errSummary(err); got != "database connection/DNS failure" {
		t.Errorf("errSummary = %q; want %q", got, "database connection/DNS failure")
	}
	// The raw pgx message is what M-8 is about: it names the connection target.
	if !strings.Contains(err.Error(), "failed to connect to") {
		t.Logf("note: pgx changed its ConnectError wording: %v", err)
	}
	assertSafeForErrorChannel(t, errSummary(err), "user=", "database=", "host=", "127.0.0.1", "failed to connect")
}

// assertSafeForErrorChannel fails if text contains any of the forbidden fragments.
func assertSafeForErrorChannel(t *testing.T, text string, forbidden ...string) {
	t.Helper()
	for _, f := range forbidden {
		if strings.Contains(text, f) {
			t.Errorf("error-channel text %q leaks %q", text, f)
		}
	}
}

// TestHardeningErrSummary locks the redacted category strings (M-8): short, no
// DSN fragment, no pgx internals, no user data.
func TestHardeningErrSummary(t *testing.T) {
	pgErr := &pgconn.PgError{
		Code:    "23505",
		Message: `duplicate key value violates unique constraint "cids_cid_key"`,
		Detail:  "Key (cid)=(secretlink) already exists.",
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"sqlstate", pgErr, "SQLSTATE 23505"},
		{"wrapped sqlstate", fmt.Errorf("exec: %w", pgErr), "SQLSTATE 23505"},
		{"timeout", fmt.Errorf("query: %w", context.DeadlineExceeded), "database timeout"},
		{"unknown", errors.New("weird db thing"), "database error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := errSummary(tc.err)
			if got != tc.want {
				t.Fatalf("errSummary = %q; want %q", got, tc.want)
			}
			assertSafeForErrorChannel(t, got, "user=", "database=", "host=", "secretlink", "cids_cid_key")
		})
	}
}

// TestHardeningHandleErrDBTimeout is the incident reproduction: a query/pool
// timeout must be handled as a DB error — "database problem" to the user, ONE
// redacted report to ERROR_CHAT_ID, NO generic incident — and it must trip the
// fast-fail gate so the queue stops paying the timeout per update.
func TestHardeningHandleErrDBTimeout(t *testing.T) {
	tgBot, fake := newHardeningFakeAPI(t)
	database := &db.DB{} // fast-fail state is a plain atomic field; nil pool is never touched
	b := &Bot{
		TG:      tgBot,
		Cfg:     &config.Config{ErrorChatID: "-100123456"},
		DB:      database,
		limiter: newSendLimiter(),
	}
	_, ctx := hardeningTestMessage(42)
	// "timeout: context deadline exceeded" is pgconn's *errTimeout; its wrapping
	// error unwraps to context.DeadlineExceeded, which is what we can construct.
	dbTimeout := fmt.Errorf("timeout: %w", context.DeadlineExceeded)

	mark := fake.mark()
	if err := b.handleErr(tgBot, ctx, dbTimeout); err != nil {
		t.Fatalf("handleErr(DB timeout) = %v; want nil so the dispatcher does NOT file a generic incident", err)
	}

	var userNotices, channelReports int
	for _, c := range fake.since(mark) {
		if c.method != "sendMessage" {
			continue
		}
		switch c.params["chat_id"] {
		case "42":
			userNotices++
			if c.params["text"] != txtDBProblem {
				t.Errorf("user notice = %q; want txtDBProblem", c.params["text"])
			}
		case "-100123456":
			channelReports++
			if !strings.Contains(c.params["text"], "PostgreSQL ERROR: database timeout") {
				t.Errorf("channel report = %q; want the redacted timeout category", c.params["text"])
			}
			assertSafeForErrorChannel(t, c.params["text"], "user=", "database=", "host=", "timeout: context")
		}
	}
	if userNotices != 1 || channelReports != 1 {
		t.Fatalf("user notices = %d, channel reports = %d; want 1 and 1", userNotices, channelReports)
	}
	// And the generic incident path was never taken: no tracking-code reply to the
	// user, no incident summary in the error chat.
	for _, c := range fake.since(mark) {
		if strings.Contains(c.params["text"], "کد پیگیری") || strings.Contains(c.params["text"], "خطایی هنگام پردازش") {
			t.Errorf("a DB timeout was filed as a generic incident: %q", c.params["text"])
		}
	}
	if !database.DegradedFor(time.Minute) {
		t.Error("the timeout class must trip the DB fast-fail gate")
	}

	// The 30 s throttle: the next failure within the window still tells the user,
	// but must not post another channel report (that is what floods during an
	// outage). Pre-set the clock instead of sleeping.
	before := fake.mark()
	if err := b.handleErr(tgBot, ctx, dbTimeout); err != nil {
		t.Fatalf("handleErr(second DB timeout) = %v; want nil", err)
	}
	var secondUser, secondChannel int
	for _, c := range fake.since(before) {
		if c.method != "sendMessage" {
			continue
		}
		switch c.params["chat_id"] {
		case "42":
			secondUser++
		case "-100123456":
			secondChannel++
		}
	}
	if secondUser != 1 || secondChannel != 0 {
		t.Errorf("second failure: user notices = %d, channel reports = %d; want 1 and 0 (throttled)", secondUser, secondChannel)
	}
}

// TestHardeningHandleErrSQLErrorNotFastFail: a PgError is the server ANSWERING.
// It gets the DB notice and a SQLSTATE report, but must not mark the database
// unavailable (other users' queries are fine, so fast-failing them would be wrong).
func TestHardeningHandleErrSQLErrorNotFastFail(t *testing.T) {
	tgBot, fake := newHardeningFakeAPI(t)
	database := &db.DB{}
	b := &Bot{TG: tgBot, Cfg: &config.Config{ErrorChatID: "-100123456"}, DB: database, limiter: newSendLimiter()}
	_, ctx := hardeningTestMessage(43)
	pgErr := &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}

	mark := fake.mark()
	if err := b.handleErr(tgBot, ctx, pgErr); err != nil {
		t.Fatalf("handleErr(PgError) = %v; want nil", err)
	}
	var channelReport string
	for _, c := range fake.since(mark) {
		if c.method == "sendMessage" && c.params["chat_id"] == "-100123456" {
			channelReport = c.params["text"]
		}
	}
	if !strings.Contains(channelReport, "PostgreSQL ERROR: SQLSTATE 23505") {
		t.Errorf("channel report = %q; want the SQLSTATE category", channelReport)
	}
	if database.DegradedFor(time.Minute) {
		t.Error("a PgError must NOT trip the fast-fail gate: the database is answering")
	}
}

// TestHardeningHandleErrConnectError: the incident's other class — the channel gets
// the redacted connection category, never the pgx message with the DB user/name.
func TestHardeningHandleErrConnectError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, connErr := pgconn.Connect(ctx, "host=127.0.0.1 port=1 connect_timeout=1")
	if connErr == nil {
		t.Fatal("connecting to 127.0.0.1:1 unexpectedly succeeded")
	}

	tgBot, fake := newHardeningFakeAPI(t)
	database := &db.DB{}
	b := &Bot{TG: tgBot, Cfg: &config.Config{ErrorChatID: "-100123456"}, DB: database, limiter: newSendLimiter()}
	_, msgCtx := hardeningTestMessage(44)

	mark := fake.mark()
	if err := b.handleErr(tgBot, msgCtx, connErr); err != nil {
		t.Fatalf("handleErr(ConnectError) = %v; want nil", err)
	}
	var report string
	for _, c := range fake.since(mark) {
		if c.method == "sendMessage" && c.params["chat_id"] == "-100123456" {
			report = c.params["text"]
		}
	}
	if !strings.Contains(report, "PostgreSQL ERROR: database connection/DNS failure") {
		t.Errorf("channel report = %q; want the connection/DNS category", report)
	}
	assertSafeForErrorChannel(t, report, "user=", "database=", "host=", "127.0.0.1", "failed to connect")
	if !database.DegradedFor(time.Minute) {
		t.Error("a failed connection must trip the DB fast-fail gate")
	}
}

// TestHardeningCanceledIsSwallowed: context.Canceled means shutdown, not failure.
// It must be a complete no-op — no user reply, no channel report, no incident.
func TestHardeningCanceledIsSwallowed(t *testing.T) {
	tgBot, fake := newHardeningFakeAPI(t)
	database := &db.DB{}
	b := &Bot{TG: tgBot, Cfg: &config.Config{ErrorChatID: "-100123456"}, DB: database, limiter: newSendLimiter()}
	_, ctx := hardeningTestMessage(45)

	mark := fake.mark()
	err := fmt.Errorf("db query: %w", context.Canceled)
	if got := b.handleErr(tgBot, ctx, err); got != nil {
		t.Fatalf("handleErr(context.Canceled) = %v; want nil (swallowed during shutdown)", got)
	}
	if calls := fake.since(mark); len(calls) != 0 {
		t.Errorf("a shutdown cancellation produced %d Bot API calls: %+v; want none", len(calls), calls)
	}
	if database.DegradedFor(time.Minute) {
		t.Error("a cancellation must not mark the database unavailable")
	}
}

// TestHardeningRealDBTimeout exercises the incident's EXACT error, pgx's
// unexported *errTimeout, against the local test database: a query whose context
// expires while the server is still executing it. Skipped when DB_HOST is unset
// (the pure-unit tests above cover the classification).
func TestHardeningRealDBTimeout(t *testing.T) {
	connString := hardeningDBConnString(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	queryCtx, queryCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer queryCancel()
	_, err = conn.Exec(queryCtx, "SELECT pg_sleep(2)")
	if err == nil {
		t.Fatal("SELECT pg_sleep(2) with a 100ms deadline did not fail")
	}
	if !pgconn.Timeout(err) {
		t.Fatalf("got %T (%v); want pgx's timeout error — this is the incident's class", err, err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		t.Fatalf("a timeout was returned as a server PgError (%v); the test does not reproduce the incident", err)
	}
	if !isDBError(err) || !isDBUnavailable(err) {
		t.Errorf("the real timeout must be classified as an unavailable DB (dbError=%v unavailable=%v)",
			isDBError(err), isDBUnavailable(err))
	}
	if got := errSummary(err); got != "database timeout" {
		t.Errorf("errSummary = %q; want %q", got, "database timeout")
	}
}

// hardeningDBConnString builds the local test-DB DSN from the same env vars the
// integration harness uses; skips the test when DB_HOST is unset.
func hardeningDBConnString(t *testing.T) string {
	t.Helper()
	host := os.Getenv("DB_HOST")
	if host == "" {
		t.Skip("DB_HOST not set; skipping live-database timeout test")
	}
	port := os.Getenv("DB_PORT")
	if port == "" {
		port = "5432"
	}
	parts := []string{
		"host=" + host,
		"port=" + port,
		"sslmode=disable",
	}
	for key, env := range map[string]string{
		"user":     "DB_USER",
		"password": "DB_PASS",
		"dbname":   "DB_NAME",
	} {
		if v := os.Getenv(env); v != "" {
			parts = append(parts, key+"="+v)
		}
	}
	return strings.Join(parts, " ")
}
