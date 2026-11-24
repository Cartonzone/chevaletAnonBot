package config

import (
	"strings"
	"testing"
	"time"
)

// clearNewOptionalEnv forces the v3.4.1 optional keys to their "unset" state, so
// a default-value assertion cannot be satisfied (or broken) by the ambient
// environment the test happens to run in.
func clearNewOptionalEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"DB_SSLMODE", "DB_CONNECT_TIMEOUT_SECONDS", "DB_MAX_CONNS",
		"HEALTH_WATCHDOG_SECONDS", "HEARTBEAT_URL",
	} {
		t.Setenv(k, "")
	}
}

// TestHardeningDefaults pins the v3.4.1 defaults: "prefer" keeps the previous
// transport behaviour, and the watchdog/connection budgets are the documented
// values (not accidental zero values that would silently disable the safety net).
func TestHardeningDefaults(t *testing.T) {
	setRequiredEnv(t)
	clearNewOptionalEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v; want nil", err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"DBSSLMode", c.DBSSLMode, "prefer"},
		{"DBConnectTimeout", c.DBConnectTimeout, 5 * time.Second},
		{"DBMaxConns", c.DBMaxConns, 20},
		{"HealthWatchdog", c.HealthWatchdog, 300 * time.Second},
		{"HeartbeatURL", c.HeartbeatURL, ""},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %v; want %v", ch.name, ch.got, ch.want)
		}
	}
}

// TestHardeningOverrides: every new knob parses from the environment.
func TestHardeningOverrides(t *testing.T) {
	setRequiredEnv(t)
	clearNewOptionalEnv(t)
	t.Setenv("DB_SSLMODE", "REQUIRE") // normalized to lower case
	t.Setenv("DB_CONNECT_TIMEOUT_SECONDS", "12")
	t.Setenv("DB_MAX_CONNS", "7")
	t.Setenv("HEALTH_WATCHDOG_SECONDS", "0") // documented "disabled" value
	t.Setenv("HEARTBEAT_URL", "https://uptime.example.com/ping/abc")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v; want nil", err)
	}
	if c.DBSSLMode != "require" {
		t.Errorf("DBSSLMode = %q; want lowercase require", c.DBSSLMode)
	}
	if c.DBConnectTimeout != 12*time.Second {
		t.Errorf("DBConnectTimeout = %s; want 12s", c.DBConnectTimeout)
	}
	if c.DBMaxConns != 7 {
		t.Errorf("DBMaxConns = %d; want 7", c.DBMaxConns)
	}
	if c.HealthWatchdog != 0 {
		t.Errorf("HealthWatchdog = %s; want 0 (disabled)", c.HealthWatchdog)
	}
	if c.HeartbeatURL != "https://uptime.example.com/ping/abc" {
		t.Errorf("HeartbeatURL = %q; want the configured URL", c.HeartbeatURL)
	}
}

// TestHardeningInvalidValues: a bad value for any new key fails the boot with an
// error naming that key, instead of silently keeping a default that hides the typo.
func TestHardeningInvalidValues(t *testing.T) {
	cases := map[string]string{
		"DB_SSLMODE":                 "yes-please",
		"DB_CONNECT_TIMEOUT_SECONDS": "0",
		"DB_MAX_CONNS":               "101",
		"HEALTH_WATCHDOG_SECONDS":    "-1",
		"HEARTBEAT_URL":              "not-a-url",
	}
	// Add the non-integer variants too.
	extra := map[string]string{
		"DB_CONNECT_TIMEOUT_SECONDS": "soon",
		"DB_MAX_CONNS":               "lots",
		"HEALTH_WATCHDOG_SECONDS":    "eventually",
		"HEARTBEAT_URL":              "ftp://example.com/ping",
	}
	run := func(t *testing.T, key, value string) {
		t.Helper()
		setRequiredEnv(t)
		clearNewOptionalEnv(t)
		t.Setenv(key, value)
		_, err := Load()
		if err == nil {
			t.Fatalf("Load() with %s=%q succeeded; want an error", key, value)
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not name %s", err, key)
		}
	}
	for key, value := range cases {
		key, value := key, value
		t.Run(key+"="+value, func(t *testing.T) { run(t, key, value) })
	}
	for key, value := range extra {
		key, value := key, value
		t.Run(key+"="+value, func(t *testing.T) { run(t, key, value) })
	}
}

// TestPortRangeValidation: DB_PORT and HEALTH_PORT were previously accepted
// verbatim and only failed much later (a dial error or a bind error). 0 and
// out-of-range values must fail the boot naming the key.
func TestPortRangeValidation(t *testing.T) {
	cases := []struct{ key, value string }{
		{"DB_PORT", "0"},
		{"DB_PORT", "65536"},
		{"HEALTH_PORT", "0"},
		{"HEALTH_PORT", "70000"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.key+"="+c.value, func(t *testing.T) {
			setRequiredEnv(t)
			clearNewOptionalEnv(t)
			t.Setenv(c.key, c.value)
			_, err := Load()
			if err == nil {
				t.Fatalf("Load() with %s=%q succeeded; want a range error", c.key, c.value)
			}
			if !strings.Contains(err.Error(), c.key) {
				t.Errorf("error %q does not name %s", err, c.key)
			}
		})
	}

	// Valid edges still load.
	setRequiredEnv(t)
	clearNewOptionalEnv(t)
	t.Setenv("DB_PORT", "65535")
	t.Setenv("HEALTH_PORT", "1")
	if _, err := Load(); err != nil {
		t.Errorf("Load() with edge-valid ports failed: %v", err)
	}
}

// TestChatIDValidation: a typo'd ERROR_CHAT_ID/REPORT_CHAT_ID used to silently
// disable error reporting; it must now fail the boot.
func TestChatIDValidation(t *testing.T) {
	for _, key := range []string{"ERROR_CHAT_ID", "REPORT_CHAT_ID"} {
		key := key
		t.Run(key+" invalid", func(t *testing.T) {
			setRequiredEnv(t)
			clearNewOptionalEnv(t)
			t.Setenv(key, "not-an-int")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("Load() with %s not-an-int = %v; want an error naming it", key, err)
			}
		})
		t.Run(key+" valid", func(t *testing.T) {
			setRequiredEnv(t)
			clearNewOptionalEnv(t)
			t.Setenv(key, "-1001234567890")
			if _, err := Load(); err != nil {
				t.Fatalf("Load() with a valid %s failed: %v", key, err)
			}
		})
	}
	// Empty means "feature off" and must stay valid.
	setRequiredEnv(t)
	clearNewOptionalEnv(t)
	t.Setenv("ERROR_CHAT_ID", "")
	t.Setenv("REPORT_CHAT_ID", "")
	if _, err := Load(); err != nil {
		t.Errorf("Load() with empty chat ids failed: %v", err)
	}
}

// TestAdminsAreTrimmed: ADMINS entries are TrimSpace'd and blanks dropped. An
// untrimmed uid would silently stop that admin's commands from working.
func TestAdminsAreTrimmed(t *testing.T) {
	setRequiredEnv(t)
	clearNewOptionalEnv(t)
	t.Setenv("ADMINS", " 111 | 222 | |333|")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"111", "222", "333"}
	if !equalStringSlicesConfig(c.Admins, want) {
		t.Errorf("Admins = %v; want %v", c.Admins, want)
	}
}

// equalStringSlicesConfig is a tiny local helper so this file does not depend on
// bot-package internals.
func equalStringSlicesConfig(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAIIntervalClamped: AI_INTERVAL is the pause between AI answers, so 0 or a
// negative value (which would busy-loop the responder) is clamped to 1 instead of
// failing the boot.
func TestAIIntervalClamped(t *testing.T) {
	for _, raw := range []string{"0", "-5"} {
		setRequiredEnv(t)
		clearNewOptionalEnv(t)
		t.Setenv("AI_INTERVAL", raw)
		c, err := Load()
		if err != nil {
			t.Fatalf("Load() with AI_INTERVAL=%s failed: %v", raw, err)
		}
		if c.AIInterval != 1 {
			t.Errorf("AI_INTERVAL=%s -> %d; want clamped to 1", raw, c.AIInterval)
		}
	}
}

// TestCIDBoundsConsistency: an inverted cid-length range or a negative default
// cid limit must fail the boot rather than rejecting every id.
func TestCIDBoundsConsistency(t *testing.T) {
	setRequiredEnv(t)
	clearNewOptionalEnv(t)
	t.Setenv("MIN_CID_LENGTH", "10")
	t.Setenv("MAX_CID_LENGTH", "5")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MIN_CID_LENGTH") {
		t.Fatalf("Load() with MIN>MAX = %v; want a MIN_CID_LENGTH error", err)
	}

	setRequiredEnv(t)
	clearNewOptionalEnv(t)
	t.Setenv("DEFAULT_CID_LIMIT", "-1")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DEFAULT_CID_LIMIT") {
		t.Fatalf("Load() with negative DEFAULT_CID_LIMIT = %v; want an error naming it", err)
	}
}
