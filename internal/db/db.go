// Package db ports modules/Global/database.py to Go using pgx.
//
// It keeps the EXACT same schema as the Python original (the bot will share the
// live production database), so MakeTables uses `CREATE TABLE IF NOT EXISTS`
// with identical column definitions — a no-op against the existing prod tables,
// and a faithful bootstrap for a fresh dev/test database.
//
// The Python DBHandler ran on a psycopg2 connection with autocommit=True, i.e.
// every statement committed immediately and there were no multi-statement
// transactions. pgxpool gives the same semantics: each method below acquires a
// pooled connection, runs one statement (implicit commit), and releases it.
package db

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aturzone/chevaletAnonBot/internal/config"
)

// Pool limits. They are intentionally NOT libpq defaults: the previous code used
// pgxpool's defaults (MaxConns = max(4, NumCPU)) which on a 2-vCPU container is 4
// — too few for the hourly jobs plus handler traffic — and outside an operator's
// control. Explicit values make the pool's footprint predictable on both the
// production host and a kubernetes-style restart.
const (
	// defaultDBMaxConns is used when config carries no value (unit tests build a
	// bare config.Config); config.Load already defaults DB_MAX_CONNS to 20.
	defaultDBMaxConns = 20
	// hardMaxDBMaxConns mirrors config's DB_MAX_CONNS ceiling. Repeated here so a
	// caller constructing Config by hand can still never exceed it.
	hardMaxDBMaxConns = 100
	// defaultDBConnectTimeout mirrors config's DB_CONNECT_TIMEOUT_SECONDS default.
	defaultDBConnectTimeout = 5 * time.Second
)

// Pool lifecycle: bound how long a connection may live/idle and how often the
// background health check reaps bad ones. A long MaxConnLifetime makes a rolling
// PostgreSQL restart (or a DNS change behind a load balancer) actually take
// effect instead of pinning every pooled connection forever.
const (
	dbMinConns          = 1
	dbMaxConnLifetime   = 30 * time.Minute
	dbMaxConnIdleTime   = 15 * time.Minute
	dbHealthCheckPeriod = 30 * time.Second
)

// IsUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505). It lets handlers reproduce the Python
// `except IntegrityError` paths (e.g. a cid rename racing another user).
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// IsNoRows reports whether err is pgx.ErrNoRows (a query that matched no row).
// The single-row getters below return it for a missing row, so callers can tell
// "no such record" apart from a genuine DB fault (connection drop, timeout) and
// route the latter to the central error/report path instead of masking it.
func IsNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// DB owns the connection pool and the per-install defaults that the Python
// schema baked into its DDL / inserts.
type DB struct {
	pool            *pgxpool.Pool
	defaultCIDLimit int
	defaultAudioTag string
	maxNameLength   int

	// lastFailureNs is the UnixNano of the most recent recorded DB failure, 0 when
	// healthy. It backs DegradedFor, the fast-fail circuit breaker that lets the
	// request path skip work while PostgreSQL is known to be down (see
	// MarkFailure/MarkSuccess). An atomic keeps the hot path lock-free: during an
	// outage every update touches this, possibly concurrently.
	lastFailureNs atomic.Int64
}

// MarkFailure records that the most recent database operation failed. It is
// deliberately cheap and silent: during an outage the error path calls it for
// every single update, so it must never log, block or allocate beyond the atomic
// store.
func (db *DB) MarkFailure() { db.lastFailureNs.Store(time.Now().UnixNano()) }

// MarkSuccess records that a database operation just succeeded, clearing any
// degraded state immediately.
func (db *DB) MarkSuccess() { db.lastFailureNs.Store(0) }

// DegradedFor reports whether the most recent recorded database outcome was a
// failure less than d ago. d <= 0 reports false. It is the primitive a request
// path can use to fail fast instead of waiting for another timeout while the
// database is known to be unreachable; it deliberately has no recovery timer —
// the first successful operation (or MarkSuccess) re-clears it.
func (db *DB) DegradedFor(d time.Duration) bool {
	if d <= 0 {
		return false
	}
	ns := db.lastFailureNs.Load()
	if ns == 0 {
		return false
	}
	return time.Since(time.Unix(0, ns)) < d
}

// Ping verifies the pool can reach PostgreSQL within ctx. It is used by startup,
// the health readiness probe and the watchdog; it does NOT touch the degraded
// state (that is driven by real query outcomes, see MarkFailure/MarkSuccess).
func (db *DB) Ping(ctx context.Context) error {
	if db == nil || db.pool == nil {
		return errors.New("db: not connected")
	}
	return db.pool.Ping(ctx)
}

// buildConnString renders the connection as a libpq-style URL.
//
// WHY A URL rather than assigning ConnConfig fields: url.UserPassword escapes the
// password (and the database name the URL parser escapes in the path), so a
// password containing '@', ':', '/', '?' or '#' no longer needs hand-rolled DSN
// quoting — the old field-assignment path only worked for "safe" secrets. The
// remaining parameters are the ones that must be explicit rather than inherited
// from an ambient PG* environment variable: the TLS mode, the client encoding
// (the Persian texts are UTF-8; a server/locale defaulting to SQL_ASCII would
// mangle them), a recognizable application_name (pgsql monitoring/locks), and a
// per-attempt connect_timeout so a dropped host cannot hang a dial for minutes.
func buildConnString(cfg *config.Config) string {
	port := cfg.DBPort
	if port == 0 {
		port = 5432
	}
	host := cfg.DBHost
	if host == "" {
		host = "localhost"
	}
	sslmode := strings.ToLower(strings.TrimSpace(cfg.DBSSLMode))
	if sslmode == "" {
		sslmode = "prefer" // libpq/pgx default; the pre-hardening behaviour
	}
	timeout := cfg.DBConnectTimeout
	if timeout <= 0 {
		timeout = defaultDBConnectTimeout
	}

	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.DBUser, cfg.DBPass),
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
	}
	if cfg.DBName != "" {
		// The URL parser percent-escapes the path, and pgx unescapes it again, so a
		// database name with unusual characters survives too.
		u.Path = "/" + cfg.DBName
	}
	q := url.Values{}
	q.Set("sslmode", sslmode)
	q.Set("client_encoding", "UTF8")
	q.Set("application_name", "chevalet-bot")
	q.Set("connect_timeout", strconv.Itoa(int(timeout/time.Second)))
	u.RawQuery = q.Encode()
	return u.String()
}

// poolConfig parses the connection URL and applies the explicit pool limits.
// Split from Connect so the connection parameters can be unit-tested without a
// live PostgreSQL.
func poolConfig(cfg *config.Config) (*pgxpool.Config, error) {
	poolCfg, err := pgxpool.ParseConfig(buildConnString(cfg))
	if err != nil {
		return nil, fmt.Errorf("db: parse config: %w", err)
	}
	maxConns := cfg.DBMaxConns
	if maxConns <= 0 {
		maxConns = defaultDBMaxConns
	}
	if maxConns > hardMaxDBMaxConns {
		maxConns = hardMaxDBMaxConns
	}
	poolCfg.MaxConns = int32(maxConns)
	poolCfg.MinConns = dbMinConns
	poolCfg.MaxConnLifetime = dbMaxConnLifetime
	poolCfg.MaxConnIdleTime = dbMaxConnIdleTime
	poolCfg.HealthCheckPeriod = dbHealthCheckPeriod
	// connect_timeout in the URL is parsed by pgx into ConnConfig.ConnectTimeout
	// already; re-assert it so a future refactor of the URL cannot drop the bound.
	poolCfg.ConnConfig.ConnectTimeout = timeoutFromConfig(cfg)
	return poolCfg, nil
}

// timeoutFromConfig returns the per-attempt connect timeout, defaulting when the
// caller passes a bare config.
func timeoutFromConfig(cfg *config.Config) time.Duration {
	if cfg.DBConnectTimeout > 0 {
		return cfg.DBConnectTimeout
	}
	return defaultDBConnectTimeout
}

// Connect opens the pool and verifies connectivity. The pool shape mirrors
// psycopg2's SimpleConnectionPool (minconn=1) except that the ceiling is
// operator-controlled: DB_MAX_CONNS, default 20, hard-capped at 100.
func Connect(ctx context.Context, cfg *config.Config) (*DB, error) {
	poolCfg, err := poolConfig(cfg)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}

	return &DB{
		pool:            pool,
		defaultCIDLimit: cfg.DefaultCIDLimit,
		defaultAudioTag: config.DefaultAudioTag,
		maxNameLength:   cfg.MaxNameLength,
	}, nil
}

// Close releases the pool.
func (db *DB) Close() { db.pool.Close() }

// Pool exposes the underlying pool for advanced callers (jobs/health).
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// MakeTables creates the four tables if they do not exist. The DDL is identical
// to DBHandler.make_tables, including the cid_limit and audio_tag defaults.
func (db *DB) MakeTables(ctx context.Context) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS users (
			id SERIAL PRIMARY KEY,
			uid VARCHAR(255) NOT NULL UNIQUE,
			name VARCHAR(255) NOT NULL,
			is_banned BOOLEAN NOT NULL DEFAULT FALSE,
			warning BOOLEAN NOT NULL DEFAULT TRUE,
			seen_option BOOLEAN NOT NULL DEFAULT FALSE,
			wpp BOOLEAN NOT NULL DEFAULT TRUE,
			cid_limit INTEGER NOT NULL DEFAULT %d,
			custom_tag VARCHAR(255),
			audio_tag VARCHAR(255) DEFAULT '%s',
			chevaletid VARCHAR(255))`, db.defaultCIDLimit, db.defaultAudioTag),
		`CREATE TABLE IF NOT EXISTS blocks (
			id SERIAL PRIMARY KEY,
			blocker_uid VARCHAR(255) NOT NULL,
			blocked_uid VARCHAR(255) NOT NULL,
			CONSTRAINT unique_pair UNIQUE (blocker_uid, blocked_uid))`,
		`CREATE TABLE IF NOT EXISTS cids (
			id SERIAL PRIMARY KEY,
			uid VARCHAR(255) NOT NULL,
			cid VARCHAR(255) NOT NULL UNIQUE)`,
		`CREATE TABLE IF NOT EXISTS reports (
			id SERIAL PRIMARY KEY,
			reported_id VARCHAR(255) NOT NULL)`,

		// report_cases backs the admin actions on a report. The reports table only
		// ever held a count per reported user, so the pair behind a report existed
		// nowhere: an admin had to copy the uid out of the channel message by hand.
		//
		// It also carries the handling state. That lives in the DB rather than in
		// the message's buttons because the action buttons are shown in an admin's
		// PRIVATE chat (channel buttons do not deliver callbacks — see
		// reportactions.go), so a keyboard can only ever reflect one admin's view.
		// Two admins opening the same report must not both ban or both count it.
		`CREATE TABLE IF NOT EXISTS report_cases (
			report_id VARCHAR(64) PRIMARY KEY,
			reporter_id VARCHAR(255) NOT NULL,
			reported_id VARCHAR(255) NOT NULL,
			channel_chat_id BIGINT,
			channel_msg_id BIGINT,
			action VARCHAR(32),
			handled_by VARCHAR(255),
			handled_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,

		// Additive migrations for features added after the original Python schema.
		// Each is idempotent (ADD COLUMN IF NOT EXISTS) and, on PostgreSQL 11+, a
		// metadata-only change (no table rewrite, no long lock) — so it is safe to
		// run on every startup against the live production database. These three
		// columns back the optional "anonymous nickname": a signature the SENDER
		// may attach to the anonymous messages they send. anon_enabled defaults to
		// FALSE so every existing and new user is opted OUT — the bot behaves
		// exactly as before until a user turns it on.
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS anon_name VARCHAR(255)`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS anon_emoji VARCHAR(255)`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS anon_enabled BOOLEAN NOT NULL DEFAULT FALSE`,

		// name_sync / tg_name / tg_name_at back "keep my display name in step with
		// my Telegram account name", so a user who renames their account does not
		// have to come here and retype it. See SyncDisplayName for the rules.
		//
		// name_sync DEFAULTS TO TRUE, so a NEW user gets it without asking. Existing
		// rows get TRUE too, but that alone changes nothing for them: SyncDisplayName
		// never renames on the first sighting of a user's account name, and opts a
		// user OUT when their stored display name has already diverged from it —
		// which is exactly the population that set a display name by hand (the
		// rename page advertises HTML-formatted names linking to a channel). So every
		// user who predates this feature keeps the name they chose — rehearsed against
		// a restored copy of production: zero renames, however the account names had
		// drifted.
		//
		// tg_name is the last account name we saw; tg_name_at is when we last looked.
		// Both are NULL on existing rows, which is what marks a user as "never seen"
		// and triggers the no-rename first sighting above.
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS name_sync BOOLEAN NOT NULL DEFAULT TRUE`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS tg_name VARCHAR(255)`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS tg_name_at TIMESTAMPTZ`,

		// Columns backing the daily admin stats (/admin_stats).
		//
		// NOTE THE MISSING DEFAULT on created_at, and that it is added in two steps.
		// Adding it WITH `DEFAULT now()` would stamp every one of the ~17k existing
		// rows with the migration time, and the first stats report would claim they
		// all joined today. Added bare, existing rows get NULL — honestly "joined
		// before tracking started" — and the default is then set so only NEW users
		// are stamped.
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ`,
		`ALTER TABLE users ALTER COLUMN created_at SET DEFAULT now()`,

		// last_active_at powers "active users today". It records only THAT a user
		// used the bot, never who they talked to — the anonymity model forbids
		// storing the pairing, and nothing here does.
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS last_active_at TIMESTAMPTZ`,
		`CREATE INDEX IF NOT EXISTS users_last_active_at_idx ON users (last_active_at)`,
		`CREATE INDEX IF NOT EXISTS users_created_at_idx ON users (created_at)`,

		// menu_bar_sent records that the persistent "🏠 منو" bar has been installed
		// in this user's chat. A ReplyKeyboard has to arrive attached to some message
		// and then persists forever, so this makes it a one-time event: without the
		// flag, either the ~17k users who joined before the bar existed would never
		// get it, or every /menu would have to send an extra message to carry it.
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS menu_bar_sent BOOLEAN NOT NULL DEFAULT FALSE`,

		// outbox holds sends that FAILED for a reason that may succeed later (a
		// Telegram rate limit, a network blip, a restart mid-send). Before it, such a
		// send was simply lost. See internal/db/outbox.go — a row exists only while a
		// delivery is outstanding and is deleted on success, so this is deliberately
		// not a record of who messaged whom.
		`CREATE TABLE IF NOT EXISTS outbox (
			id BIGSERIAL PRIMARY KEY,
			chat_id BIGINT NOT NULL,
			method VARCHAR(64) NOT NULL,
			params JSONB NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			next_try TIMESTAMPTZ NOT NULL DEFAULT now(),
			created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE INDEX IF NOT EXISTS outbox_next_try_idx ON outbox (next_try)`,

		// daily_metrics is a pure per-day COUNTER. Deliberately not a message log:
		// counting deliveries needs no sender, no recipient and no content, so the
		// bot can report volume without recording who messaged whom.
		`CREATE TABLE IF NOT EXISTS daily_metrics (
			day DATE PRIMARY KEY,
			messages BIGINT NOT NULL DEFAULT 0)`,

		// message_box is the "message box" queue: when the recipient has it OFF
		// (the default), delivered messages are also stored here so they can be
		// replayed to the recipient as one batch later instead of arriving as a
		// stream of interruptions.
		//
		// PRIVACY MODEL: this table stores delivery REFERENCES (source chat /
		// message ids) plus the metadata needed to reproduce the send — never the
		// message text and never its media content. The bot re-copies the original
		// message from Telegram by id at flush time; the body never enters this DB.
		// Rows are deleted once delivered (or capped away), so it is not a message
		// archive.
		`CREATE TABLE IF NOT EXISTS message_box (
			id BIGSERIAL PRIMARY KEY,
			recipient_uid VARCHAR(255) NOT NULL,
			sender_uid VARCHAR(255) NOT NULL,
			source_chat_id BIGINT NOT NULL,
			source_message_id BIGINT NOT NULL,
			media_group_id VARCHAR(64),
			kind VARCHAR(32) NOT NULL,
			target_cid VARCHAR(255),
			reply_to_chat BIGINT NOT NULL DEFAULT 0,
			reply_to_message BIGINT NOT NULL DEFAULT 0,
			quote_text TEXT,
			quote_position INTEGER NOT NULL DEFAULT 0,
			external_reply BOOLEAN NOT NULL DEFAULT FALSE,
			channel_reply BOOLEAN NOT NULL DEFAULT FALSE,
			status VARCHAR(16) NOT NULL DEFAULT 'pending',
			claim VARCHAR(64),
			created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,

		// The delivery worker claims pending rows for one recipient in insertion
		// order (claim/backoff columns above), so the index matches that query.
		`CREATE INDEX IF NOT EXISTS message_box_recipient_idx ON message_box (recipient_uid, status, id)`,

		// The per-user opt-in. OFF BY DEFAULT (hard requirement): existing and new
		// users keep the plain immediate-delivery behaviour until they turn it on.
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS message_box BOOLEAN NOT NULL DEFAULT FALSE`,
	}
	for _, s := range stmts {
		if _, err := db.pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("db: make tables: %w", err)
		}
	}
	return nil
}

// --- shared scan helpers ------------------------------------------------------

// queryOptString returns "" when the row is missing OR the column is NULL,
// matching the Python getters that returned None in both cases.
func (db *DB) queryOptString(ctx context.Context, sql string, args ...any) (string, error) {
	var s *string
	err := db.pool.QueryRow(ctx, sql, args...).Scan(&s)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if s == nil {
		return "", nil
	}
	return *s, nil
}

// queryBool scans a single boolean. ErrNoRows propagates as an error because the
// callers (settings/getters) only run for users known to exist (prep ensures
// the row via init_user), matching the Python code that would raise on a missing
// row.
func (db *DB) queryBool(ctx context.Context, sql string, args ...any) (bool, error) {
	var b bool
	err := db.pool.QueryRow(ctx, sql, args...).Scan(&b)
	return b, err
}

// truncateRunes mirrors Python's `str(name)[:MAX_NAME_LENGTH]` (slicing by code
// point, not byte).
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
