package health

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// get drives the handler and returns status + body for one request.
func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", path, err)
	}
	return resp.StatusCode, string(body)
}

// TestHealthzLiveness covers the liveness contract: 200 "ok" when the injected
// liveness says the process is working, 503 with the reason when it says not.
func TestHealthzLiveness(t *testing.T) {
	// nil Checks is the lenient mode: alive == ok.
	if code, body := get(t, NewHandler(Checks{}), "/healthz"); code != 200 || body != "ok" {
		t.Errorf("/healthz with no checks = %d %q; want 200 \"ok\"", code, body)
	}

	var ok atomic.Bool
	h := NewHandler(Checks{Liveness: func() (bool, string) {
		if ok.Load() {
			return true, ""
		}
		return false, "no successful getUpdates for 4m0s"
	}})
	if code, body := get(t, h, "/healthz"); code != 503 || !strings.Contains(body, "getUpdates") {
		t.Errorf("/healthz when not live = %d %q; want 503 naming the reason", code, body)
	}
	ok.Store(true)
	if code, body := get(t, h, "/healthz"); code != 200 || body != "ok" {
		t.Errorf("/healthz when live = %d %q; want 200 \"ok\"", code, body)
	}

	// /health is the legacy BASE_ADDRESS alias and must behave identically.
	if code, body := get(t, h, "/health"); code != 200 || body != "ok" {
		t.Errorf("/health alias = %d %q; want 200 \"ok\"", code, body)
	}
}

// TestReadyz covers readiness: it needs liveness AND the readiness check; the
// reason text must say which one failed.
func TestReadyz(t *testing.T) {
	ready := func(context.Context) (bool, string) { return true, "" }
	notReady := func(context.Context) (bool, string) { return false, "database ping failed: dial tcp: refused" }

	h := NewHandler(Checks{Readiness: ready})
	if code, body := get(t, h, "/readyz"); code != 200 || body != "ok" {
		t.Errorf("/readyz ready = %d %q; want 200 \"ok\"", code, body)
	}

	h = NewHandler(Checks{Liveness: func() (bool, string) { return false, "polling stalled" }, Readiness: ready})
	if code, body := get(t, h, "/readyz"); code != 503 || body != "polling stalled" {
		t.Errorf("/readyz not live = %d %q; want 503 \"polling stalled\"", code, body)
	}

	h = NewHandler(Checks{Readiness: notReady})
	if code, body := get(t, h, "/readyz"); code != 503 || !strings.Contains(body, "database ping failed") {
		t.Errorf("/readyz not ready = %d %q; want 503 with the DB reason", code, body)
	}
}

// TestReadyzTimeout: a hung readiness check (a wedged DB ping) must be cut off at
// ReadyTimeout and reported as 503, never hang the probe.
func TestReadyzTimeout(t *testing.T) {
	blocked := func(ctx context.Context) (bool, string) {
		<-ctx.Done() // the DB ping outliving its budget
		return false, "database ping timed out"
	}
	h := NewHandler(Checks{Readiness: blocked, ReadyTimeout: 50 * time.Millisecond})

	start := time.Now()
	code, body := get(t, h, "/readyz")
	elapsed := time.Since(start)
	if code != 503 || !strings.Contains(body, "timed out") {
		t.Errorf("/readyz with a hung check = %d %q; want 503", code, body)
	}
	if elapsed > time.Second {
		t.Errorf("/readyz with a hung check took %s; ReadyTimeout was not enforced", elapsed)
	}
}

// TestHealthMethodRestricted: only GET/HEAD are meaningful; anything else is a
// 405 rather than being silently treated as a probe.
func TestHealthMethodRestricted(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Checks{}))
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d; want 405", resp.StatusCode)
	}
}

// TestUnknownPathIs404: an unknown path must not fall through to an "ok" body
// (a typo'd probe would otherwise look healthy).
func TestUnknownPathIs404(t *testing.T) {
	if code, _ := get(t, NewHandler(Checks{}), "/healtz"); code != http.StatusNotFound {
		t.Errorf("GET /healtz = %d; want 404", code)
	}
}
