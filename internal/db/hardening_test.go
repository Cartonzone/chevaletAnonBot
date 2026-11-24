package db

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aturzone/chevaletAnonBot/internal/config"
)

// TestBuildConnStringURL locks the URL-based connection string: the password and
// user (and database name) must be escaped by url.UserPassword / the URL parser,
// so a secret containing DSN metacharacters no longer needs hand-rolled quoting,
// and the required libpq parameters are present.
func TestBuildConnStringURL(t *testing.T) {
	cfg := &config.Config{
		DBHost:           "db.example.com",
		DBPort:           6543,
		DBName:           "chevalet prod", // space exercises path escaping
		DBUser:           "bot user",
		DBPass:           "p@ss:w/rd?#%",
		DBSSLMode:        "require",
		DBConnectTimeout: 7 * time.Second,
	}
	raw := buildConnString(cfg)

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("buildConnString produced an unparseable URL %q: %v", raw, err)
	}
	if u.Scheme != "postgres" {
		t.Errorf("scheme = %q; want postgres", u.Scheme)
	}
	if u.Hostname() != "db.example.com" || u.Port() != "6543" {
		t.Errorf("host = %q port = %q; want db.example.com:6543", u.Hostname(), u.Port())
	}
	if got := u.User.Username(); got != cfg.DBUser {
		t.Errorf("username round-trip = %q; want %q (must be escaped)", got, cfg.DBUser)
	}
	if got, _ := u.User.Password(); got != cfg.DBPass {
		t.Errorf("password round-trip = %q; want %q (url.UserPassword must escape it)", got, cfg.DBPass)
	}
	if got := strings.TrimPrefix(u.Path, "/"); got != cfg.DBName {
		t.Errorf("database round-trip = %q; want %q", got, cfg.DBName)
	}

	q := u.Query()
	for key, want := range map[string]string{
		"sslmode":          "require",
		"client_encoding":  "UTF8",
		"application_name": "chevalet-bot",
		"connect_timeout":  "7",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("%s = %q; want %q", key, got, want)
		}
	}

	// Empty optional fields fall back to the documented defaults rather than
	// emitting "sslmode=" / "connect_timeout=0".
	raw = buildConnString(&config.Config{DBHost: "localhost"})
	u, err = url.Parse(raw)
	if err != nil {
		t.Fatalf("default buildConnString unparseable: %v", err)
	}
	if got := u.Query().Get("sslmode"); got != "prefer" {
		t.Errorf("default sslmode = %q; want prefer", got)
	}
	if got := u.Query().Get("connect_timeout"); got != "5" {
		t.Errorf("default connect_timeout = %q; want 5", got)
	}

	// An empty DB_NAME (config leaves it optional) must still produce a URL pgx
	// accepts; PostgreSQL then defaults the database to the user name, exactly as
	// the old ConnConfig assignment did.
	if _, err := poolConfig(&config.Config{DBHost: "localhost"}); err != nil {
		t.Errorf("poolConfig with an empty DB name failed: %v", err)
	}
}

// TestPoolConfigConnFields verifies the URL is actually parsed back by pgx into
// the expected connection fields — including the escaped password and the
// sslmode-driven TLS configuration.
func TestPoolConfigConnFields(t *testing.T) {
	cfg := &config.Config{
		DBHost:           "127.0.0.1",
		DBPort:           55432,
		DBName:           "chevalet_test",
		DBUser:           "root",
		DBPass:           "testpass",
		DBSSLMode:        "prefer",
		DBConnectTimeout: 3 * time.Second,
		DBMaxConns:       7,
	}
	pc, err := poolConfig(cfg)
	if err != nil {
		t.Fatalf("poolConfig: %v", err)
	}
	cc := pc.ConnConfig
	if cc.Host != "127.0.0.1" || cc.Port != 55432 || cc.Database != "chevalet_test" {
		t.Errorf("conn = %s:%d/%s; want 127.0.0.1:55432/chevalet_test", cc.Host, cc.Port, cc.Database)
	}
	if cc.User != "root" || cc.Password != "testpass" {
		t.Errorf("credentials = %q/%q; want root/testpass", cc.User, cc.Password)
	}
	if cc.RuntimeParams["client_encoding"] != "UTF8" {
		t.Errorf("client_encoding = %q; want UTF8", cc.RuntimeParams["client_encoding"])
	}
	if cc.RuntimeParams["application_name"] != "chevalet-bot" {
		t.Errorf("application_name = %q; want chevalet-bot", cc.RuntimeParams["application_name"])
	}
	if cc.ConnectTimeout != 3*time.Second {
		t.Errorf("ConnectTimeout = %s; want 3s (from connect_timeout)", cc.ConnectTimeout)
	}
	// sslmode=prefer: TLS is attempted first, with a plaintext fallback.
	if cc.TLSConfig == nil || len(cc.Fallbacks) == 0 {
		t.Errorf("sslmode=prefer: TLSConfig=%v fallbacks=%d; want TLS-then-plaintext", cc.TLSConfig, len(cc.Fallbacks))
	}

	// sslmode=disable must not carry a TLS config at all.
	cfg.DBSSLMode = "disable"
	pc, err = poolConfig(cfg)
	if err != nil {
		t.Fatalf("poolConfig(disable): %v", err)
	}
	if pc.ConnConfig.TLSConfig != nil || len(pc.ConnConfig.Fallbacks) != 0 {
		t.Errorf("sslmode=disable still configured TLS (tls=%v fallbacks=%d)",
			pc.ConnConfig.TLSConfig, len(pc.ConnConfig.Fallbacks))
	}
}

// TestPoolSettings locks the explicit pool limits (previously pgx defaults, i.e.
// ~4 connections on a 2-vCPU container, with no lifetime bounds).
func TestPoolSettings(t *testing.T) {
	base := &config.Config{DBHost: "127.0.0.1", DBPort: 5432, DBName: "x", DBUser: "u", DBPass: "p"}

	pc, err := poolConfig(base) // no DBMaxConns -> default 20
	if err != nil {
		t.Fatal(err)
	}
	if pc.MaxConns != 20 {
		t.Errorf("default MaxConns = %d; want 20", pc.MaxConns)
	}
	if pc.MinConns != 1 {
		t.Errorf("MinConns = %d; want 1", pc.MinConns)
	}
	if pc.MaxConnLifetime != 30*time.Minute {
		t.Errorf("MaxConnLifetime = %s; want 30m", pc.MaxConnLifetime)
	}
	if pc.MaxConnIdleTime != 15*time.Minute {
		t.Errorf("MaxConnIdleTime = %s; want 15m", pc.MaxConnIdleTime)
	}
	if pc.HealthCheckPeriod != 30*time.Second {
		t.Errorf("HealthCheckPeriod = %s; want 30s", pc.HealthCheckPeriod)
	}

	base.DBMaxConns = 1000 // above the hard cap: clamped, never rejected at this layer
	pc, err = poolConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	if pc.MaxConns != 100 {
		t.Errorf("MaxConns with 1000 = %d; want clamped to 100", pc.MaxConns)
	}

	base.DBMaxConns = 3
	pc, err = poolConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	if pc.MaxConns != 3 {
		t.Errorf("MaxConns with 3 = %d; want 3", pc.MaxConns)
	}
}

// TestDegradedFastFail locks the circuit-breaker primitive: a failure marks the DB
// degraded for the window, a success clears it immediately, and a non-positive
// window is never degraded.
func TestDegradedFastFail(t *testing.T) {
	var database DB // fields only; the pool is never touched by these methods

	if database.DegradedFor(30 * time.Second) {
		t.Error("a fresh DB reports degraded; want healthy")
	}
	if database.DegradedFor(0) || database.DegradedFor(-time.Second) {
		t.Error("DegradedFor(<=0) must be false")
	}

	database.MarkFailure()
	if !database.DegradedFor(30 * time.Second) {
		t.Error("after MarkFailure DegradedFor(30s) = false; want true")
	}
	// The window expires.
	time.Sleep(2 * time.Millisecond)
	if database.DegradedFor(time.Microsecond) {
		t.Error("DegradedFor(1µs) stayed true 2ms after the failure; the window did not expire")
	}

	// A success clears it immediately, without waiting for the window.
	database.MarkFailure()
	database.MarkSuccess()
	if database.DegradedFor(time.Hour) {
		t.Error("after MarkSuccess the DB still reports degraded; success must clear it")
	}
}

// TestConnectTimeoutBlackhole proves a connect attempt against an unroutable
// address is bounded by the configured budget instead of hanging. TEST-NET-1
// (192.0.2.1) is reserved for documentation and is never a real database: the
// attempt either fails fast (no route/refused) or is cut off by connect_timeout.
func TestConnectTimeoutBlackhole(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network-dependent connect-timeout test in -short mode")
	}
	cfg := &config.Config{
		DBHost:           "192.0.2.1",
		DBPort:           65000,
		DBName:           "nope",
		DBUser:           "nope",
		DBPass:           "nope",
		DBSSLMode:        "disable",
		DBConnectTimeout: 500 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	database, err := Connect(ctx, cfg)
	elapsed := time.Since(start)
	if err == nil {
		database.Close()
		t.Fatal("Connect to a TEST-NET address succeeded; the test environment is unusable for this check")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Connect took %s; the connect budget (%s) was not enforced", elapsed, cfg.DBConnectTimeout)
	}
}

// TestPingAgainstLocalPostgres is the integration sanity check for the added
// Ping method: with DB_HOST set it connects and pings, then reports nil. Skipped
// without a database (matching the rest of this package's integration tests).
func TestPingAgainstLocalPostgres(t *testing.T) {
	cfg := testConfig(t) // skips when DB_HOST is unset
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	database, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(database.Close)

	if err := database.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
