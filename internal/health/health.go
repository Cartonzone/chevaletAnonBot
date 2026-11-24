// Package health serves the bot's HTTP liveness and readiness endpoints.
//
// HISTORY: this used to be a bare TCP listener that accepted and immediately
// closed a connection, mirroring the Python health_check_app. That only proved
// the PROCESS was alive: Docker's `nc -z` probe stayed green while long polling
// was wedged or PostgreSQL was gone, so a dead bot was never restarted. The
// endpoints here report the two facts an orchestrator actually needs:
//
//	GET /healthz (alias /health) — liveness: the process is alive AND updates are
//	    flowing (or we are still inside the startup grace). A failure means "kill
//	    and restart this container".
//	GET /readyz — readiness: /healthz AND the database answers a ping quickly AND
//	    the runtime assets were validated at startup. A failure means "do not send
//	    me traffic yet" (during boot) or "I cannot serve right now".
//
// The package owns no policy: every signal comes from the injected Checks, so the
// bot's own counters and the DB pool stay the single source of truth and the
// handlers are trivially testable.
package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// DefaultReadyTimeout bounds a /readyz check. Readiness is probed by an
// orchestrator with its own short deadline; the DB ping must answer well inside
// it, or the probe is reported as not ready instead of hanging.
const DefaultReadyTimeout = 1500 * time.Millisecond

// server timeouts. The handlers are trivial, so these only stop a stuck or
// malicious local connection from pinning a goroutine forever.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 5 * time.Second
	writeTimeout      = 5 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 3 * time.Second
)

// Checks carries the runtime signals behind the endpoints. Every function is
// optional; a nil one is treated as healthy, which lets a caller that only wants
// a liveness socket (tests, a future probe) start a listener without wiring
// anything.
type Checks struct {
	// Liveness reports whether the process is doing its job. The bot wires this
	// to its polling tracker (a successful getUpdates within the last 90s, or the
	// startup grace window).
	Liveness func() (ok bool, reason string)
	// Readiness reports whether the bot can serve traffic right now. It is only
	// consulted after Liveness passed. ctx is bounded by ReadyTimeout.
	Readiness func(ctx context.Context) (ok bool, reason string)
	// ReadyTimeout overrides DefaultReadyTimeout when non-zero.
	ReadyTimeout time.Duration
}

// livenessOK evaluates the liveness gate.
func (c Checks) livenessOK() (bool, string) {
	if c.Liveness == nil {
		return true, ""
	}
	ok, reason := c.Liveness()
	if ok {
		return true, ""
	}
	if reason == "" {
		reason = "unhealthy"
	}
	return false, reason
}

// readyTimeout returns the configured (or default) readiness budget.
func (c Checks) readyTimeout() time.Duration {
	if c.ReadyTimeout > 0 {
		return c.ReadyTimeout
	}
	return DefaultReadyTimeout
}

// readinessOK evaluates readiness: liveness first, then the caller's check under
// a bounded context.
func (c Checks) readinessOK(parent context.Context) (bool, string) {
	if ok, reason := c.livenessOK(); !ok {
		return false, reason
	}
	if c.Readiness == nil {
		return true, ""
	}
	ctx, cancel := context.WithTimeout(parent, c.readyTimeout())
	defer cancel()
	ok, reason := c.Readiness(ctx)
	if ok {
		return true, ""
	}
	if reason == "" {
		reason = "not ready"
	}
	return false, reason
}

// NewHandler builds the health mux. Exposed separately from Listen so tests (and
// any caller that wants to mount the endpoints on its own server) can drive it
// directly.
func NewHandler(checks Checks) http.Handler {
	mux := http.NewServeMux()
	writeStatus := func(w http.ResponseWriter, ok bool, reason string) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// Never let a proxy cache a health verdict.
		w.Header().Set("Cache-Control", "no-store")
		if ok {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(reason))
	}
	methodOK := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return false
		}
		return true
	}
	liveness := func(w http.ResponseWriter, r *http.Request) {
		if !methodOK(w, r) {
			return
		}
		ok, reason := checks.livenessOK()
		writeStatus(w, ok, reason)
	}
	mux.HandleFunc("/healthz", liveness)
	// /health is the Python BRAIN's BASE_ADDRESS default and is kept as an alias so
	// an existing probe or bookmarked URL keeps working through the cutover.
	mux.HandleFunc("/health", liveness)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !methodOK(w, r) {
			return
		}
		ok, reason := checks.readinessOK(r.Context())
		writeStatus(w, ok, reason)
	})
	return mux
}

// Listener owns the health HTTP server.
type Listener struct {
	srv *http.Server
	ln  net.Listener
}

// Listen starts the health HTTP server on 127.0.0.1:port in a background
// goroutine and returns immediately, so a caller can open the probe endpoint
// BEFORE the database is reachable (it then reports "not ready" until the
// readiness checks pass).
//
// The address is the IPv4 loopback EXPLICITLY (not "localhost"): in a dual-stack
// container /etc/hosts often maps "localhost" to ::1 as well, and Go's net.Listen
// binds a single resolved address — so "localhost" could bind only [::1]:port
// while the Docker healthcheck (wget 127.0.0.1) probes IPv4, marking a live bot
// unhealthy and restart-looping it at cutover. The Python original pinned IPv4
// the same way (socket.AF_INET + bind("localhost", …)).
func Listen(port int, checks Checks) (*Listener, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("health: listen %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           NewHandler(checks),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	h := &Listener{srv: srv, ln: ln}
	go h.serve()
	return h, nil
}

func (h *Listener) serve() {
	if err := h.srv.Serve(h.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Warn("health: server stopped unexpectedly", "err", err)
	}
}

// Close stops the server, waiting briefly for in-flight probes to finish.
func (h *Listener) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := h.srv.Shutdown(ctx); err != nil {
		return h.srv.Close() // force-close if a probe is stuck
	}
	return nil
}
