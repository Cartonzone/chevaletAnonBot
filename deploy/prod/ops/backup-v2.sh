#!/usr/bin/env bash
# Chevalet DB backup v2 — dump → verify → publish → manifest → retention → alert.
#
# Replaces the legacy backup script family whose `docker exec -t` merged pg_dump
# stderr into the gzip stream (a 130-byte gzip containing an error message passes
# `gzip -t` and could be kept as a "backup") and which wrote world-readable files
# with no checksum, no structure check and no failure alerting.
#
# Guarantees:
#   * `docker exec -i` only — NEVER `-t` (no TTY in cron/systemd; -t corrupts the
#     piped dump with CRLF and interleaves stderr).
#   * The dump is written to a temp file, verified, and only then atomically
#     renamed into published/ — readers/drills never see a partial artifact.
#   * Verified = size >= MIN_BYTES, `gzip -t`, pg_dump footer line, >= MIN_TABLES
#     `CREATE TABLE` blocks, sha256 manifest written next to it.
#   * Retention: hourly for 48 h, daily for 14 d, weekly for 8 w (by artifact age);
#     the newest artifact is always kept.
#   * Any failure runs ALERT_CMD (e.g. ops/alert.sh) and exits non-zero.
#
# Modes:
#   backup-v2.sh           normal backup run (used by chevalet-backup.timer)
#   backup-v2.sh --gate    pre-deploy gate: NO dump is taken; exits 0 only if the
#                          newest published artifact is verified and < 90 min old.
#
# All configuration is environment variables (systemd Environment/EnvironmentFile,
# never a sourced .env — the old scripts ran `source <(grep … .env)`, which both
# truncates values containing '#' and can execute shell from the file):
#   DB_CONTAINER=telegram-bot-db   DB_NAME=mydatabase   DB_USER=root
#       DB_USER must match the container's POSTGRES_USER (the dump authenticates
#       with the CONTAINER's POSTGRES_PASSWORD, so no DB password is in our env).
#   BACKUP_ROOT=/opt/bots/chevalet/backups-v2
#   ALERT_CMD='bash /opt/bots/chevalet/ops/alert.sh'   (optional)
#   HC_PING_URL=<healthchecks.io URL>                  (optional dead-man switch)
#   GATE_MAX_AGE_MINUTES=90                            (--gate only)
#
# Exit codes: 0 success/skip-nothing-to-do, 1 failure, 2 usage/config error.
set -Eeuo pipefail
IFS=$'\n\t'
umask 077

SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

MODE="run"
case "${1:-}" in
    "") MODE="run" ;;
    --gate) MODE="gate" ;;
    -h | --help)
        sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
        exit 0
        ;;
    *)
        echo "usage: $0 [--gate]" >&2
        exit 2
        ;;
esac

DB_CONTAINER="${DB_CONTAINER:-telegram-bot-db}"
DB_NAME="${DB_NAME:-mydatabase}"
DB_USER="${DB_USER:-root}"
BACKUP_ROOT="${BACKUP_ROOT:-/opt/bots/chevalet/backups-v2}"
# Default to the sibling alert.sh so a configured ALERT_TOKEN/ALERT_CHAT_ID is
# enough to get failure alerts without extra wiring.
ALERT_CMD="${ALERT_CMD:-bash ${SELF_DIR}/alert.sh}"
HC_PING_URL="${HC_PING_URL:-}"
GATE_MAX_AGE_MINUTES="${GATE_MAX_AGE_MINUTES:-90}"

DUMP_TIMEOUT_SECONDS=300
MIN_BYTES=1000000   # ~1 MB: production dump is ~2.8 MB; anything smaller is suspect
MIN_TABLES=8        # users, blocks, cids, reports, report_cases, outbox, daily_metrics, message_box

STAGING="$BACKUP_ROOT/staging"
PUBLISHED="$BACKUP_ROOT/published"
LOG_DIR="$BACKUP_ROOT/log"
LOG_FILE="$LOG_DIR/backup.log"
LOCK_FILE="${LOCK_FILE:-$BACKUP_ROOT/.backup.lock}"
ERRLOG="$STAGING/dump.err"

ALERTED=0
STAGING_DUMP=""

log() {
    printf '%s %s\n' "$(date -u +%FT%TZ)" "$*" | tee -a "$LOG_FILE"
}

run_alert() {
    local message="$1"
    ALERTED=1
    log "ALERT: $message"
    if [ -z "$ALERT_CMD" ]; then
        log "ALERT_CMD not configured — alert only logged"
        return 0
    fi
    local -a alert_cmd
    # IFS is globally newline/tab in these scripts; give read an explicit space
    # IFS so an ALERT_CMD like "bash /path/alert.sh" splits into words.
    IFS=' ' read -r -a alert_cmd <<<"$ALERT_CMD"
    "${alert_cmd[@]}" "$message" || log "ALERT_CMD failed"
    return 0
}

die() {
    local message="$1"
    log "ERROR: $message"
    run_alert "$message"
    exit 1
}

cleanup() {
    local rc=$?
    set +e
    trap - EXIT
    [ -n "$STAGING_DUMP" ] && rm -f -- "$STAGING_DUMP"
    rm -f -- "$ERRLOG"
    if [ "$rc" -ne 0 ] && [ "$ALERTED" -eq 0 ]; then
        local detail=""
        if [ -s "$ERRLOG" ]; then
            detail=": $(tail -n 3 "$ERRLOG" | tr '\n' ' ' | head -c 500)"
        fi
        run_alert "backup failed (rc=${rc})${detail}"
    fi
    exit "$rc"
}
trap cleanup EXIT

mkdir -p "$STAGING" "$PUBLISHED" "$LOG_DIR"
chmod 700 "$BACKUP_ROOT" "$STAGING" "$PUBLISHED" "$LOG_DIR" 2>/dev/null || true
touch "$LOG_FILE"
chmod 600 "$LOG_FILE" 2>/dev/null || true

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------
validate_identifier() {
    # DB_NAME / DB_USER are interpolated into `docker exec … sh -c`. Restrict to
    # identifier characters so a compromised environment cannot inject shell.
    local name="$1" value="$2"
    case "$value" in
        "" | *[!A-Za-z0-9_-]*)
            log "ERROR: ${name} must be a plain identifier, got: ${value}"
            return 1
            ;;
    esac
    return 0
}

verify_artifact() {
    # $1 = path to *.sql.gz; checks integrity + structure. Returns 0 only when the
    # file is safe to publish/restore. Does not require the .sha256 file.
    local artifact="$1" size tables
    if [ ! -s "$artifact" ]; then
        log "verify: missing or empty artifact: $artifact"
        return 1
    fi
    size=$(stat -c %s "$artifact")
    if [ "$size" -lt "$MIN_BYTES" ]; then
        log "verify: artifact too small (${size} B < ${MIN_BYTES} B)"
        return 1
    fi
    if ! gzip -t "$artifact" 2>/dev/null; then
        log "verify: gzip -t failed (truncated/corrupt gzip stream)"
        return 1
    fi
    # NOTE: grep -c (not -q): with `pipefail`, grep -q exiting early on a match
    # makes gzip die of SIGPIPE and the pipeline would look failed.
    footer_count=$(gzip -dc "$artifact" 2>/dev/null | grep -cF -- '-- PostgreSQL database dump complete' || true)
    if [ "${footer_count:-0}" -lt 1 ]; then
        log "verify: pg_dump completion footer missing"
        return 1
    fi
    tables=$(gzip -dc "$artifact" 2>/dev/null | grep -c '^CREATE TABLE ' || true)
    if [ "${tables:-0}" -lt "$MIN_TABLES" ]; then
        log "verify: found ${tables:-0} CREATE TABLE blocks (< ${MIN_TABLES}); wrong database or truncated dump"
        return 1
    fi
    return 0
}

verify_checksum_file() {
    # $1 = artifact path; if <artifact>.sha256 exists, check it matches.
    local artifact="$1" sha_file="${1}.sha256" dir base
    [ -f "$sha_file" ] || return 0
    dir=$(dirname "$artifact")
    base=$(basename "$artifact")
    if ! (cd "$dir" && sha256sum -c --status "${base}.sha256"); then
        log "verify: sha256 mismatch for $artifact"
        return 1
    fi
    return 0
}

deadman_ping() {
    [ -n "$HC_PING_URL" ] || return 0
    if ! curl -fsS --max-time 10 --retry 2 --retry-connrefused "$HC_PING_URL" >/dev/null 2>&1; then
        log "WARN: dead-man switch ping failed (${HC_PING_URL%%\?*}…) — not failing the backup"
    fi
    return 0
}

newest_artifact() {
    # Prints the newest published *.sql.gz by filename (UTC timestamp names sort
    # chronologically), or nothing.
    local files
    files=$(find "$PUBLISHED" -maxdepth 1 -type f -name 'backup_*.sql.gz' -printf '%f\n' 2>/dev/null | sort | tail -n 1)
    [ -n "$files" ] || return 0
    printf '%s/%s\n' "$PUBLISHED" "$files"
}

prune_retention() {
    # Deterministic, age-based retention:
    #   <= 48 h              keep all          (hourly tier)
    #   48 h .. 14 d         keep newest/day   (daily tier)
    #   14 d .. 8 w          keep newest/week  (weekly tier)
    #   older                delete
    # The newest artifact overall is ALWAYS kept, even if the clock is skewed.
    local now files n i f age day week deleted kept
    now=$(date +%s)
    mapfile -t files < <(find "$PUBLISHED" -maxdepth 1 -type f -name 'backup_*.sql.gz' -printf '%T@ %f\n' 2>/dev/null | sort -rn | cut -d' ' -f2-)
    n=${#files[@]}
    if [ "$n" -eq 0 ]; then
        log "retention: no artifacts"
        return 0
    fi
    declare -A seen_day=() seen_week=()
    deleted=0
    kept=1 # files[0] = newest, always kept
    for ((i = 1; i < n; i++)); do
        f="$PUBLISHED/${files[$i]}"
        age=$((now - $(stat -c %Y "$f")))
        if [ "$age" -le $((48 * 3600)) ]; then
            kept=$((kept + 1))
            continue
        fi
        if [ "$age" -le $((14 * 86400)) ]; then
            day=$(date -u -d "@$(stat -c %Y "$f")" +%Y-%m-%d)
            if [ -n "${seen_day[$day]:-}" ]; then
                rm -f -- "$f" "${f}.sha256" "${f}.manifest.json"
                deleted=$((deleted + 1))
            else
                seen_day[$day]=1
                kept=$((kept + 1))
            fi
            continue
        fi
        if [ "$age" -le $((56 * 86400)) ]; then
            week=$(date -u -d "@$(stat -c %Y "$f")" +%G-W%V)
            if [ -n "${seen_week[$week]:-}" ]; then
                rm -f -- "$f" "${f}.sha256" "${f}.manifest.json"
                deleted=$((deleted + 1))
            else
                seen_week[$week]=1
                kept=$((kept + 1))
            fi
            continue
        fi
        rm -f -- "$f" "${f}.sha256" "${f}.manifest.json"
        deleted=$((deleted + 1))
    done
    log "retention: ${n} artifacts scanned, ${kept} kept, ${deleted} pruned (hourly<=48h, daily<=14d, weekly<=8w)"
}

# ---------------------------------------------------------------------------
# --gate: pre-deploy check, read-only
# ---------------------------------------------------------------------------
if [ "$MODE" = "gate" ]; then
    newest=$(newest_artifact)
    if [ -z "$newest" ]; then
        die "pre-deploy gate FAILED: no published backup artifacts in ${PUBLISHED}"
    fi
    if ! verify_artifact "$newest" || ! verify_checksum_file "$newest"; then
        die "pre-deploy gate FAILED: newest artifact is not verified: $(basename "$newest")"
    fi
    age_min=$((($(date +%s) - $(stat -c %Y "$newest")) / 60))
    if [ "$age_min" -gt "$GATE_MAX_AGE_MINUTES" ]; then
        die "pre-deploy gate FAILED: newest verified artifact is ${age_min} min old (> ${GATE_MAX_AGE_MINUTES} min): $(basename "$newest")"
    fi
    log "pre-deploy gate OK: $(basename "$newest") is ${age_min} min old and verified"
    echo "GATE OK: $(basename "$newest") ${age_min}m old"
    exit 0
fi

# ---------------------------------------------------------------------------
# normal run
# ---------------------------------------------------------------------------
validate_identifier DB_NAME "$DB_NAME" || exit 2
validate_identifier DB_USER "$DB_USER" || exit 2

# Serialize runs: an overlapping timer/manual run must not race on staging names
# or publish two dumps with the same second-stamp.
exec 9>"$LOCK_FILE"
if ! flock -n 9; then
    log "another backup run holds ${LOCK_FILE}; skipping"
    run_alert "backup skipped: a previous run is still holding the lock (stuck pg_dump?)"
    exit 0
fi

if [ "$(docker inspect --format '{{.State.Running}}' "$DB_CONTAINER" 2>/dev/null || echo false)" != "true" ]; then
    die "backup FAILED: database container ${DB_CONTAINER} is not running"
fi

stamp=$(date -u +%Y%m%dT%H%M%SZ)
base="backup_${DB_NAME}_${stamp}.sql.gz"
tmp="$STAGING/${base}.part"
STAGING_DUMP="$tmp"
rm -f -- "$tmp" "$ERRLOG"

log "backup start: db=${DB_NAME} container=${DB_CONTAINER} -> ${PUBLISHED}/${base}"

# 1) Dump WITHOUT a TTY, stderr captured separately, bounded in time.
if ! timeout --signal=TERM --kill-after=30 "$DUMP_TIMEOUT_SECONDS" \
    docker exec -i "$DB_CONTAINER" sh -c \
    "PGPASSWORD=\$POSTGRES_PASSWORD pg_dump -U ${DB_USER} -d ${DB_NAME} --no-owner --no-privileges --encoding=UTF8" \
    2>"$ERRLOG" | gzip -6 >"$tmp"; then
    die "backup FAILED: pg_dump/gzip pipeline failed for ${DB_NAME} (see ${ERRLOG})"
fi

# 2) Verify BEFORE publishing — never rename an unverified file into published/.
if ! verify_artifact "$tmp"; then
    die "backup FAILED: new dump did not pass verification"
fi

size=$(stat -c %s "$tmp")
sha=$(sha256sum "$tmp" | cut -d' ' -f1)
tables=$(gzip -dc "$tmp" | grep -c '^CREATE TABLE ' || true)
pg_dump_version=$(docker exec "$DB_CONTAINER" pg_dump --version 2>/dev/null | head -n 1 || true)
live_users=$(docker exec "$DB_CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -tAc 'select count(*) from users' 2>/dev/null | tr -d '[:space:]' || true)
hostname_short=$(hostname -s)
published="$PUBLISHED/$base"

# 3) Publish atomically + write the certification files next to the artifact.
chmod 600 "$tmp"
mv -f -- "$tmp" "$published"
STAGING_DUMP=""
chmod 600 "$published"
printf '%s  %s\n' "$sha" "$base" >"$published.sha256"
chmod 600 "$published.sha256"
cat >"$published.manifest.json" <<EOF
{
  "format": "chevalet-backup-v2",
  "created_utc": "${stamp}",
  "host": "${hostname_short}",
  "container": "${DB_CONTAINER}",
  "db": "${DB_NAME}",
  "db_user": "${DB_USER}",
  "dump_format": "plain-sql",
  "compression": "gzip",
  "size_bytes": ${size},
  "tables": ${tables},
  "live_users": ${live_users:-0},
  "sha256": "${sha}",
  "pg_dump_version": "${pg_dump_version:-unknown}"
}
EOF
chmod 600 "$published.manifest.json"

if ! verify_checksum_file "$published"; then
    rm -f -- "$published" "$published.sha256" "$published.manifest.json"
    die "backup FAILED: published artifact failed its own sha256 check"
fi

# 4) Retention (hourly 48 h / daily 14 d / weekly 8 w). Newest is always kept.
prune_retention

log "backup ok: ${base} size=${size}B tables=${tables} users=${live_users:-?} sha=${sha:0:12}…"
deadman_ping
exit 0
