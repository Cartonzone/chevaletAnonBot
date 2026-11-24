// Command bot is the Go entrypoint for ChevaletAnonBot — the in-progress port
// of the Python bot (which lives on the `python` branch). See MIGRATION.md.
//
// Startup order matters as much as what runs:
//
//	health endpoint  -> FIRST, so the container probe answers "starting" while the
//	                    database is still being reached (Docker's start_period
//	                    alone is not enough once a connect loop can take a minute)
//	database         -> bounded retry; on failure exit(1) so the restart policy
//	                    makes the outage visible instead of running half-alive
//	runtime assets   -> fail closed on missing texts/backups/unwritable dynset
//	bot + polling    -> the bot's own Run blocks until SIGTERM/SIGINT
//	watchdog/heartbeat -> tracked goroutines that may exit(1) on a silent stall
//
// A health listener that is up before the DB is what lets /healthz say "alive"
// while /readyz says "not ready" through a slow start.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // embed the IANA tz database so Asia/Tehran (GM/GN) resolves without OS tzdata

	"github.com/aturzone/chevaletAnonBot/internal/bot"
	"github.com/aturzone/chevaletAnonBot/internal/config"
	"github.com/aturzone/chevaletAnonBot/internal/db"
	"github.com/aturzone/chevaletAnonBot/internal/health"
	"github.com/aturzone/chevaletAnonBot/internal/texts"
)

// Startup/shutdown budgets and probe thresholds.
const (
	// startupGrace is how long /healthz stays green before it demands a successful
	// getUpdates: the process needs time to connect, validate assets and complete
	// its first long poll. Matches the health contract (and the watchdog's own
	// grace) at 2 minutes.
	startupGrace = 2 * time.Minute

	// pollStaleAfter is the liveness threshold: a successful getUpdates older than
	// this means updates have silently stopped arriving (wedged poll loop, dead
	// proxy) even though the process is alive.
	pollStaleAfter = 90 * time.Second

	// Database startup retry: 5 attempts, exponential backoff 2s/4s/8s/16s, all
	// inside a hard 60s total budget. This rides out a PostgreSQL that is still
	// starting (compose service_healthy helps, but a rolling DB restart does not)
	// without hiding a permanently misconfigured DB — after the budget the process
	// exits and Docker's restart policy makes it visible.
	dbConnectAttempts = 5
	dbConnectBudget   = 60 * time.Second
	dbConnectBackoff  = 2 * time.Second
	// dbSchemaTimeout bounds the MakeTables DDL: ADD COLUMN IF NOT EXISTS can wait
	// on a lock held by another connection, and a boot must not hang on it.
	dbSchemaTimeout = 60 * time.Second

	// Watchdog: after startupGrace, check every 30s; exit(1) when updates have
	// stopped for HEALTH_WATCHDOG_SECONDS or the DB ping fails 3 times in a row.
	// Docker's restart policy heals the process; that is safer than running wedged.
	watchdogInterval      = 30 * time.Second
	watchdogDBPingTimeout = 1 * time.Second
	watchdogDBFailures    = 3

	// heartbeatInterval/timeout are the optional HEARTBEAT_URL uptime ping.
	heartbeatInterval = 60 * time.Second
	heartbeatTimeout  = 4 * time.Second

	// healthDBPingTimeout bounds the /readyz database ping specifically, inside the
	// health package's 1.5s readiness budget.
	healthDBPingTimeout = 1 * time.Second
	// trackedShutdownTimeout bounds waiting for our own watchdog/heartbeat
	// goroutines after the bot has drained.
	trackedShutdownTimeout = 5 * time.Second
)

func main() {
	cfg, err := config.Load()
	slog.SetDefault(newLogger(os.Getenv("LOG_LEVEL")))
	if err != nil {
		slog.Error("failed to load configuration", "err", err)
		os.Exit(1)
	}

	if err := run(cfg); err != nil {
		// A signal during startup (SIGTERM while the database is still being
		// reached) is a clean stop, not a crash: do not page anyone with exit code 1.
		if errors.Is(err, context.Canceled) {
			slog.Info("startup interrupted by a signal; stopping")
			return
		}
		slog.Error("bot stopped with error", "err", err)
		os.Exit(1)
	}
	slog.Info("shut down cleanly")
}

// run is main's body. It returns an error (instead of calling os.Exit) so the
// deferred database/health close really run on every non-watchdog exit path.
func run(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("starting ChevaletAnonBot (Go)",
		"bot_id", cfg.BotID,
		"db_host", cfg.DBHost,
		"db_name", cfg.DBName,
		"db_sslmode", cfg.DBSSLMode,
		"db_max_conns", cfg.DBMaxConns,
		"health_port", cfg.HealthPort,
		"watchdog", cfg.HealthWatchdog,
		"admins", len(cfg.Admins),
	)

	state := newRuntimeState()

	// 1. Health FIRST. During the DB retry window /healthz answers 200 ("process
	// alive, starting") and /readyz answers 503 ("not ready yet"), which is exactly
	// what Docker needs to avoid killing the container before it has had a chance.
	hl, err := health.Listen(cfg.HealthPort, health.Checks{
		Liveness:     state.liveness,
		Readiness:    state.readiness,
		ReadyTimeout: health.DefaultReadyTimeout,
	})
	if err != nil {
		return fmt.Errorf("start health endpoint: %w", err)
	}
	defer hl.Close()
	slog.Info("health endpoint listening", "port", cfg.HealthPort, "liveness", "/healthz", "readiness", "/readyz")

	// 2. Database, with a bounded retry budget, then the idempotent schema DDL.
	database, err := connectDBWithRetry(ctx, cfg)
	if err != nil {
		return err
	}
	defer database.Close()
	state.setDB(database)

	schemaCtx, cancelSchema := context.WithTimeout(ctx, dbSchemaTimeout)
	err = database.MakeTables(schemaCtx)
	cancelSchema()
	if err != nil {
		return fmt.Errorf("ensure schema: %w", err)
	}
	slog.Info("database ready")

	// 3. Runtime assets: fail closed BEFORE polling starts, with an error naming the
	// exact path. A missing Texts/ (wrong image/volume), unwritable dynset path or
	// unreadable backups dir is a deployment bug that would otherwise surface only
	// when a user hits that feature.
	if err := validateAssets(cfg, "Texts", "backups"); err != nil {
		state.setAssets(err)
		return fmt.Errorf("runtime assets: %w", err)
	}
	state.setAssets(nil)

	// 4. The bot itself (also validates BOT_TOKEN via getMe and the token cipher).
	txt := texts.New("Texts")
	b, err := bot.New(cfg, database, txt)
	if err != nil {
		return fmt.Errorf("initialise bot: %w", err)
	}
	state.setBot(b)

	// 5. Auxiliary tracked goroutines: the watchdog (restart-on-stall) and the
	// optional external heartbeat. They run on a context we can cancel as soon as
	// Run returns (Run can return early on an error before SIGTERM), so shutdown
	// never waits out the watchdog's startup grace.
	auxCtx, cancelAux := context.WithCancel(ctx)
	defer cancelAux()
	var aux sync.WaitGroup
	aux.Add(1)
	go func() {
		defer aux.Done()
		watchdog(auxCtx, cfg, state)
	}()
	if cfg.HeartbeatURL != "" {
		aux.Add(1)
		go func() {
			defer aux.Done()
			heartbeatLoop(auxCtx, cfg.HeartbeatURL)
		}()
	}

	// 6. Block on long polling; Run drains its own updater/batcher/background work
	// on cancellation before returning.
	runErr := b.Run(auxCtx)

	// Run has returned (normally on SIGTERM, or early on an error): stop and reap
	// the auxiliary goroutines, then take the health endpoint down BEFORE the
	// deferred database close so no probe can observe a closed pool.
	cancelAux()
	waitTracked(&aux, trackedShutdownTimeout)
	_ = hl.Close() // also deferred; closing twice is safe and this fixes the order
	return runErr
}

// runtimeState is the process state the health endpoints can inspect. It exists
// so the listener can start BEFORE the database/bot do: every accessor is nil-safe
// and guarded, because the HTTP handlers run concurrently with startup.
type runtimeState struct {
	mu       sync.RWMutex
	start    time.Time
	database *db.DB
	bot      *bot.Bot
	// assetsReady is set only after validateAssets passed; until then /readyz
	// reports why (assetsProblem).
	assetsReady   bool
	assetsProblem string
}

func newRuntimeState() *runtimeState { return &runtimeState{start: time.Now()} }

func (s *runtimeState) setDB(database *db.DB) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.database = database
}

func (s *runtimeState) setBot(b *bot.Bot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bot = b
}

// setAssets records the outcome of validateAssets: nil means ready, an error means
// /readyz returns it as the reason.
func (s *runtimeState) setAssets(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.assetsReady = true
		s.assetsProblem = ""
		return
	}
	s.assetsReady = false
	s.assetsProblem = err.Error()
}

// liveness implements /healthz: process alive AND updates flowing (or inside the
// startup grace).
func (s *runtimeState) liveness() (bool, string) {
	s.mu.RLock()
	b, started := s.bot, s.start
	s.mu.RUnlock()
	if b == nil {
		if time.Since(started) <= startupGrace {
			return true, "starting"
		}
		return false, "bot not initialised"
	}
	return pollLiveness(time.Now(), started, b.PollStatus())
}

// pollLiveness is the pure policy behind /healthz, kept separate so it can be
// tested without a running bot: alive while inside the startup grace, then
// healthy only while the last successful getUpdates is recent.
func pollLiveness(now, started time.Time, st bot.PollStatus) (bool, string) {
	if now.Sub(started) <= startupGrace {
		return true, "starting"
	}
	if st.LastSuccess.IsZero() {
		return false, "no successful getUpdates since start"
	}
	if idle := now.Sub(st.LastSuccess); idle > pollStaleAfter {
		return false, fmt.Sprintf("no successful getUpdates for %s", idle.Round(time.Second))
	}
	return true, ""
}

// readiness implements /readyz: assets validated AND the database answers a ping
// quickly. Liveness is checked by the health package before this runs.
func (s *runtimeState) readiness(ctx context.Context) (bool, string) {
	s.mu.RLock()
	database, ready, problem := s.database, s.assetsReady, s.assetsProblem
	s.mu.RUnlock()
	if !ready {
		if problem == "" {
			problem = "runtime assets not validated yet"
		}
		return false, problem
	}
	if database == nil {
		return false, "database not connected"
	}
	pingCtx, cancel := context.WithTimeout(ctx, healthDBPingTimeout)
	defer cancel()
	if err := database.Ping(pingCtx); err != nil {
		return false, "database ping failed: " + err.Error()
	}
	return true, ""
}

// connectDBWithRetry opens the pool within a bounded total budget, using the
// production retry parameters (see connectDBRetry).
func connectDBWithRetry(ctx context.Context, cfg *config.Config) (*db.DB, error) {
	return connectDBRetry(ctx, cfg, dbConnectAttempts, dbConnectBudget, dbConnectBackoff)
}

// connectDBRetry opens the pool, retrying up to attempts times inside budget.
// Each attempt is itself bounded by the per-attempt DB_CONNECT_TIMEOUT_SECONDS.
// A blackholed database therefore cannot block the boot forever, and a
// permanently misconfigured one ends in an error instead of a half-alive process.
func connectDBRetry(ctx context.Context, cfg *config.Config, attempts int, budget, backoff time.Duration) (*db.DB, error) {
	deadline := time.Now().Add(budget)
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		attemptCtx, cancel := context.WithTimeout(ctx, remaining)
		database, err := db.Connect(attemptCtx, cfg)
		cancel()
		if err == nil {
			if attempt > 1 {
				slog.Info("database connected after retry", "attempt", attempt)
			}
			return database, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt == attempts || time.Now().Add(backoff).After(deadline) {
			break
		}
		slog.Warn("database connection failed; retrying",
			"attempt", attempt, "of", attempts, "backoff", backoff, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	if lastErr == nil {
		lastErr = errors.New("connect budget exhausted before any attempt")
	}
	return nil, fmt.Errorf("database unreachable after %d attempt(s) within %s: %w", attempts, budget, lastErr)
}

// validateAssets checks the runtime files the bot cannot work without. Every
// failure is fatal at boot: these are deployment errors (a missing image content,
// an unmounted volume, a read-only state dir), and running without them means
// broken user-visible behavior that looks like a code bug later.
func validateAssets(cfg *config.Config, textsDir, backupsDir string) error {
	if err := validateTextsDir(textsDir); err != nil {
		return err
	}
	if err := validateDynsetPath(cfg.DynsetPath); err != nil {
		return err
	}
	return validateBackupsDir(backupsDir)
}

// validateTextsDir requires the Texts directory to exist and hold at least one
// non-empty .txt template: the bot's every user-facing message comes from there,
// so an empty (or missing) directory means a bot that cannot speak.
func validateTextsDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("texts directory %q: %w (the Dockerfile copies Texts to /app/Texts; a bind-mount must not shadow it)", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("texts path %q is not a directory", dir)
	}
	templates := 0
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".txt") {
			return nil
		}
		info, ierr := entry.Info()
		if ierr != nil {
			return ierr
		}
		if info.Size() > 0 {
			templates++
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("read texts directory %q: %w", dir, err)
	}
	if templates == 0 {
		return fmt.Errorf("texts directory %q contains no non-empty .txt template", dir)
	}
	slog.Info("texts validated", "dir", dir, "templates", templates)
	return nil
}

// validateDynsetPath proves dynamic_settings.json's parent directory is writable
// with a real create+write+remove. Checking permissions with os.Stat is not
// enough: the path is often a Docker bind-mounted FILE whose parent exists but is
// read-only, and every admin ai-url/donate change would then be silently lost.
func validateDynsetPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("DYNSET_PATH is empty")
	}
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("DYNSET_PATH parent %q is not creatable: %w", dir, err)
	}
	// The target itself must be a writable FILE, not a directory. This is the
	// classic Docker bind-mount failure: mounting a file path that does not exist
	// on the host makes Docker create a DIRECTORY at the container path, every
	// dynset save then fails and the admin's donation/AI settings are silently
	// lost on the next restart. Checking the parent alone cannot see that.
	if info, statErr := os.Stat(path); statErr == nil {
		if info.IsDir() {
			return fmt.Errorf("DYNSET_PATH %q is a directory (a bind-mounted settings file is missing on the host?)", path)
		}
		target, openErr := os.OpenFile(path, os.O_WRONLY, 0) // no create/truncate: do not touch existing settings
		if openErr != nil {
			return fmt.Errorf("DYNSET_PATH %q is not writable: %w", path, openErr)
		}
		_ = target.Close()
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("DYNSET_PATH %q: %w", path, statErr)
	}
	f, err := os.CreateTemp(dir, ".dynset-writecheck-*")
	if err != nil {
		return fmt.Errorf("DYNSET_PATH parent %q is not writable: %w", dir, err)
	}
	name := f.Name()
	_, writeErr := f.WriteString("ok")
	closeErr := f.Close()
	_ = os.Remove(name)
	if writeErr != nil {
		return fmt.Errorf("DYNSET_PATH parent %q is not writable: %w", dir, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("DYNSET_PATH parent %q: %w", dir, closeErr)
	}
	slog.Info("dynamic settings path is writable", "path", path)
	return nil
}

// validateBackupsDir requires the backups directory to exist and be readable (the
// nightly delivery and /admin backup both read it), and logs the newest dump's
// age so a stalled backup pipeline is visible at every boot. An empty directory is
// fine (a fresh install has no dump yet) but a missing/unreadable one is fatal —
// the configured backup feature could never work.
func validateBackupsDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("backups directory %q: %w (mount it at /app/backups; the nightly delivery and /admin backup read it)", dir, err)
	}
	var newest string
	var newestAt time.Time
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql.gz") {
			continue
		}
		info, ierr := entry.Info()
		if ierr != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestAt) {
			newest, newestAt = entry.Name(), info.ModTime()
		}
	}
	if newest == "" {
		slog.Warn("backups directory has no *.sql.gz dump yet", "dir", dir)
		return nil
	}
	slog.Info("backups directory readable",
		"dir", dir, "newest", newest, "age", time.Since(newestAt).Round(time.Minute))
	return nil
}

// watchdog is the in-process stall detector. After the startup grace it checks
// every 30s: have updates arrived recently, and is the database still reachable?
//
// It calls os.Exit(1) on a stall on purpose: a wedged process cannot recover
// itself, but Docker's restart policy can replace it, and a crash-looping
// container is far more visible than a bot that answers health probes while
// silently delivering nothing. HEALTH_WATCHDOG_SECONDS=0 disables it.
func watchdog(ctx context.Context, cfg *config.Config, state *runtimeState) {
	if cfg.HealthWatchdog <= 0 {
		slog.Info("watchdog disabled", "reason", "HEALTH_WATCHDOG_SECONDS=0")
		return
	}
	slog.Info("watchdog armed",
		"startup_grace", startupGrace, "interval", watchdogInterval,
		"poll_stale_after", cfg.HealthWatchdog, "db_failure_threshold", watchdogDBFailures)

	select {
	case <-ctx.Done():
		return
	case <-time.After(startupGrace):
	}

	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()
	dbFailures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		state.mu.RLock()
		b, database := state.bot, state.database
		state.mu.RUnlock()
		if b == nil || database == nil {
			continue // still booting; the startup grace should have covered this
		}

		lastOK := b.LastPollOK()
		if lastOK.IsZero() {
			slog.Error("watchdog: no successful getUpdates since start; exiting so the container restarts")
			os.Exit(1)
		}
		if idle := time.Since(lastOK); idle > cfg.HealthWatchdog {
			slog.Error("watchdog: polling is stalled; exiting so the container restarts",
				"last_success_ago", idle.Round(time.Second), "limit", cfg.HealthWatchdog)
			os.Exit(1)
		}

		pingCtx, cancel := context.WithTimeout(ctx, watchdogDBPingTimeout)
		err := database.Ping(pingCtx)
		cancel()
		if err != nil {
			dbFailures++
			slog.Warn("watchdog: database ping failed",
				"consecutive", dbFailures, "threshold", watchdogDBFailures, "err", err)
			if dbFailures >= watchdogDBFailures {
				slog.Error("watchdog: database unreachable; exiting so the container restarts",
					"consecutive_failures", dbFailures)
				os.Exit(1)
			}
			continue
		}
		dbFailures = 0
	}
}

// heartbeatLoop GETs the optional HEARTBEAT_URL immediately and then every 60s,
// so an external uptime monitor notices this bot going dark. It is deliberately
// quiet on failure (debug/warn only): a monitoring ping failing must never
// interfere with the bot's real jobs.
func heartbeatLoop(ctx context.Context, url string) {
	client := &http.Client{Timeout: heartbeatTimeout}
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		heartbeatOnce(ctx, client, url)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// heartbeatOnce performs a single heartbeat GET, bounded by client.Timeout and
// the context. It never returns an error: every failure is logged at most at warn.
func heartbeatOnce(ctx context.Context, client *http.Client, url string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		slog.Debug("heartbeat: cannot build request", "err", err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("heartbeat: request failed", "err", err)
		return
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the connection can be reused; the body is unused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 400 {
		slog.Warn("heartbeat: endpoint returned an error status", "status", resp.StatusCode)
	}
}

// waitTracked waits for auxiliary goroutines, bounded so a stuck one cannot block
// process exit.
func waitTracked(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn("auxiliary goroutines did not finish within the shutdown budget", "timeout", timeout)
	}
}

// newLogger maps the Python LOG_LEVEL names onto slog levels.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "DEBUG":
		lvl = slog.LevelDebug
	case "WARNING", "WARN":
		lvl = slog.LevelWarn
	case "ERROR", "CRITICAL":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
