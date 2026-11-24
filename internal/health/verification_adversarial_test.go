package health

// Adversarial verification tests for the v3.4.1 health endpoints (task-7).
//
// These are DELIBERATELY separate from health_hardening_test.go: they exercise the
// real HTTP listener (not just the mux) and pin the contract Docker/systemd and the
// off-host watchdog depend on:
//
//   - a live listener answers /healthz even while /readyz is 503 (startup window);
//   - HEAD is accepted and carries no body; POST/PUT are 405 with an Allow header;
//   - the 503 body is the injected reason (so a human probing the port can see WHY);
//   - readiness is not consulted when liveness already failed;
//   - Close is idempotent (main.go calls it twice on purpose) and probes after
//     Close fail instead of hanging.

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// freePort grabs an ephemeral loopback port and returns it. There is a tiny race
// between closing the listener and Listen re-binding it, but nothing else in the
// test binary binds ports, so it is safe here.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func doRequest(t *testing.T, method, url string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s body: %v", method, url, err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

// TestVerificationListenerStartupWindow drives a REAL listener through the boot
// window: alive but not ready, then fully ready. This is the exact sequence Docker's
// healthcheck and the /readyz poller see during startup.
func TestVerificationListenerStartupWindow(t *testing.T) {
	var ready atomic.Bool
	port := freePort(t)
	ln, err := Listen(port, Checks{
		Liveness: func() (bool, string) { return true, "" }, // inside the startup grace
		Readiness: func(ctx context.Context) (bool, string) {
			if ready.Load() {
				return true, ""
			}
			return false, "runtime assets not validated yet"
		},
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	base := "http://127.0.0.1:" + itoa(port)

	// Liveness must be green while readiness is still 503: a healthcheck on /healthz
	// must not kill a slow-booting container.
	if code, _, body := doRequest(t, "GET", base+"/healthz"); code != 200 || body != "ok" {
		t.Fatalf("boot-window /healthz = %d %q; want 200 ok", code, body)
	}
	if code, _, body := doRequest(t, "GET", base+"/readyz"); code != 503 || !strings.Contains(body, "assets") {
		t.Fatalf("boot-window /readyz = %d %q; want 503 naming the assets", code, body)
	}

	ready.Store(true)
	if code, _, body := doRequest(t, "GET", base+"/readyz"); code != 200 || body != "ok" {
		t.Fatalf("ready /readyz = %d %q; want 200 ok", code, body)
	}
}

// TestVerificationListenerHTTPContract pins method handling and headers on the real
// listener: HEAD is allowed (no body), unsupported methods are 405 with Allow, the
// verdict is never cached, and the legacy /health alias answers.
func TestVerificationListenerHTTPContract(t *testing.T) {
	port := freePort(t)
	ln, err := Listen(port, Checks{
		Liveness:  func() (bool, string) { return false, "polling wedged" },
		Readiness: func(ctx context.Context) (bool, string) { return true, "" },
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	base := "http://127.0.0.1:" + itoa(port)

	code, hdr, body := doRequest(t, "HEAD", base+"/healthz")
	if code != 503 {
		t.Errorf("HEAD /healthz = %d; want 503 (liveness failed)", code)
	}
	if body != "" {
		t.Errorf("HEAD /healthz returned a %d-byte body; net/http must suppress it", len(body))
	}
	if cc := hdr.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q; want no-store (a proxy must never cache a health verdict)", cc)
	}

	for _, method := range []string{"POST", "PUT", "DELETE"} {
		code, hdr, _ := doRequest(t, method, base+"/healthz")
		if code != http.StatusMethodNotAllowed {
			t.Errorf("%s /healthz = %d; want 405", method, code)
		}
		if allow := hdr.Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "HEAD") {
			t.Errorf("%s Allow header = %q; want GET, HEAD", method, allow)
		}
	}

	// /health is the legacy BASE_ADDRESS alias: it must return the same verdict and
	// the same reason body, not 404 (an existing probe URL keeps working).
	code, _, body = doRequest(t, "GET", base+"/health")
	if code != 503 || !strings.Contains(body, "polling wedged") {
		t.Errorf("/health alias = %d %q; want 503 with the liveness reason", code, body)
	}
}

// TestVerificationReadinessSkippedWhenUnhealthy proves /readyz short-circuits on
// liveness: a wedged bot must not spend its readiness budget (and a DB ping) when
// the answer is already "restart me".
func TestVerificationReadinessSkippedWhenUnhealthy(t *testing.T) {
	var readinessCalls atomic.Int32
	h := NewHandler(Checks{
		Liveness: func() (bool, string) { return false, "polling wedged" },
		Readiness: func(ctx context.Context) (bool, string) {
			readinessCalls.Add(1)
			return true, ""
		},
	})
	code, body := get(t, h, "/readyz")
	if code != 503 || !strings.Contains(body, "polling wedged") {
		t.Fatalf("/readyz with a dead liveness = %d %q; want 503 with the liveness reason", code, body)
	}
	if n := readinessCalls.Load(); n != 0 {
		t.Errorf("readiness check ran %d time(s) while liveness was failing; want 0", n)
	}
}

// TestVerificationListenerConcurrentProbes runs probes concurrently under -race, then
// closes the listener twice (main.go does exactly that: deferred Close + an explicit
// Close to fix ordering).
func TestVerificationListenerConcurrentProbes(t *testing.T) {
	port := freePort(t)
	ln, err := Listen(port, Checks{
		Liveness:  func() (bool, string) { return true, "" },
		Readiness: func(ctx context.Context) (bool, string) { return true, "" },
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	base := "http://127.0.0.1:" + itoa(port)

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := "/healthz"
			if i%2 == 1 {
				path = "/readyz"
			}
			resp, err := http.Get(base + path)
			if err != nil {
				t.Errorf("concurrent GET %s: %v", path, err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("concurrent GET %s = %d; want 200", path, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()

	if err := ln.Close(); err != nil {
		t.Errorf("first Close = %v; want nil", err)
	}
	if err := ln.Close(); err != nil {
		t.Errorf("second Close = %v; want nil (main.go closes twice on purpose)", err)
	}
	// After Close the probes must fail rather than hang; a short client timeout keeps
	// this deterministic.
	client := &http.Client{Timeout: time.Second}
	if resp, err := client.Get(base + "/healthz"); err == nil {
		_ = resp.Body.Close()
		t.Errorf("GET after Close still answered %d; the listener should be down", resp.StatusCode)
	}
}

// itoa avoids importing strconv twice across the file's helper section.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
