package health

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestHealthListener locks the IPv4-loopback bind contract (the HIGH audit fix:
// bind 127.0.0.1, not "localhost" which can resolve to ::1 on a dual-stack host
// while the Docker healthcheck probes 127.0.0.1), the HTTP liveness endpoint that
// replaced the old accept-then-close TCP socket, and that the port is
// exclusively held.
func TestHealthListener(t *testing.T) {
	const port = 39517
	// A nil-check Checks is the lenient mode: process alive == healthy.
	hl, err := Listen(port, Checks{})
	if err != nil {
		t.Skipf("port %d unavailable in this environment: %v", port, err)
	}
	defer hl.Close()

	// the IPv4 loopback MUST answer — this is exactly what the Docker healthcheck
	// (wget http://127.0.0.1:${HEALTH_PORT}/healthz) relies on.
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		t.Fatalf("GET 127.0.0.1:%d/healthz failed: %v (the listener must bind the IPv4 loopback)", port, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("GET /healthz = %d %q; want 200 \"ok\"", resp.StatusCode, body)
	}

	// the port is exclusively held: a second Listen on it must fail.
	if hl2, err := Listen(port, Checks{}); err == nil {
		_ = hl2.Close()
		t.Error("a second Listen on the same port should fail (already bound)")
	}
}
