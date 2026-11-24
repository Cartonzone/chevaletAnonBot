#!/usr/bin/env bash
# Chevalet restore drill — prove the newest VERIFIED backup actually restores.
#
# Starts a throwaway `postgres:16-alpine` container (random name, random loopback
# port, no persistent volume), restores the artifact with
# `psql -v ON_ERROR_STOP=1`, asserts the data invariants, prints a PASS/FAIL line
# with the duration, writes a dated drill record, and always destroys the
# container. Local Docker only: this NEVER points at production.
#
# Integrity gate BEFORE any Docker/PG work: sha256 (when present), gzip -t, the
# pg_dump completion footer, >= MIN_TABLES CREATE TABLE blocks, and NO
# cluster-level DDL. The last one is a data-loss gate: legacy `pg_dump --create`
# artifacts contain DROP/CREATE DATABASE + \connect, which restore-v2 prod must
# refuse; a drill PASS on an artifact prod would reject would be misleading, so
# this fails with the explicit conversion pointer instead.
#
# Invariants asserted (all must hold):
#   * >= MIN_TABLES base tables in the restored database (default 8)
#   * users > 0
#   * cids >= users            (every user gets at least one cid)
#   * 0 users with a non-numeric uid
#   * 0 duplicate cids
#   * users count within DRILL_USER_TOLERANCE_PCT of the manifest's live count
#
# Usage:
#   restore-drill.sh                # newest published artifact
#   restore-drill.sh <artifact>     # explicit artifact
#
# Environment:
#   BACKUP_ROOT=/opt/bots/chevalet/backups-v2
#   PG_IMAGE=postgres:16-alpine
#   DRILL_USER_TOLERANCE_PCT=5
#   ALERT_CMD='bash /opt/bots/chevalet/ops/alert.sh'
#   DRILL_TIMEOUT_SECONDS=900
#
# Exit codes: 0 PASS, 1 FAIL.
set -Eeuo pipefail
IFS=$'\n\t'
umask 077

SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
BACKUP_ROOT="${BACKUP_ROOT:-/opt/bots/chevalet/backups-v2}"
PUBLISHED="$BACKUP_ROOT/published"
DRILL_RECORD_DIR="${DRILL_RECORD_DIR:-$BACKUP_ROOT/drills}"
LOG_DIR="$BACKUP_ROOT/log"
LOG_FILE="$LOG_DIR/drill.log"
LOCK_FILE="${LOCK_FILE:-$BACKUP_ROOT/.drill.lock}"
PG_IMAGE="${PG_IMAGE:-postgres:16-alpine}"
MIN_TABLES="${MIN_TABLES:-8}"
DRILL_USER_TOLERANCE_PCT="${DRILL_USER_TOLERANCE_PCT:-5}"
DRILL_TIMEOUT_SECONDS="${DRILL_TIMEOUT_SECONDS:-900}"
ALERT_CMD="${ALERT_CMD:-bash ${SELF_DIR}/alert.sh}"

ALERTED=0
DRILL_NAME=""
DRILL_CREATED=0
WORK_DIR=""
DRILL_PASSWORD=""
ARTIFACT=""
START_EPOCH=0

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
    RESULT_REASON="$message"
    log "FAIL: $message"
    finish FAIL
}

finish() {
    # $1 = PASS|FAIL. Writes the drill record, removes the container and exits.
    local result="$1" duration user_msg
    duration=$(($(date +%s) - START_EPOCH))
    user_msg="restore drill ${result}: artifact=$(basename "${ARTIFACT:-?}") duration=${duration}s"
    if [ -n "${RESULT_REASON:-}" ]; then
        user_msg="${user_msg} reason=${RESULT_REASON}"
    fi
    mkdir -p "$DRILL_RECORD_DIR" 2>/dev/null || true
    local record
    record="$DRILL_RECORD_DIR/drill_$(date -u +%Y%m%dT%H%M%SZ)_${result}.json"
    cat >"$record" <<EOF 2>/dev/null || true
{
  "format": "chevalet-drill-v1",
  "result": "${result}",
  "created_utc": "$(date -u +%FT%TZ)",
  "artifact": "$(basename "${ARTIFACT:-unknown}")",
  "host": "$(hostname -s)",
  "duration_seconds": ${duration},
  "tables": ${COUNT_TABLES:-0},
  "users": ${COUNT_USERS:-0},
  "cids": ${COUNT_CIDS:-0},
  "bad_uids": ${COUNT_BAD_UIDS:-0},
  "dup_cids": ${COUNT_DUP_CIDS:-0},
  "reason": "${RESULT_REASON:-}"
}
EOF
    chmod 600 "$record" 2>/dev/null || true
    if [ "$result" = "PASS" ]; then
        printf '%s artifact=%s tables=%s users=%s cids=%s\n' \
            "$(date -u +%FT%TZ)" "$(basename "$ARTIFACT")" "$COUNT_TABLES" "$COUNT_USERS" "$COUNT_CIDS" \
            >"$DRILL_RECORD_DIR/last-PASS"
        chmod 600 "$DRILL_RECORD_DIR/last-PASS" 2>/dev/null || true
        log "RESULT: PASS ${user_msg#restore drill PASS: }"
        echo "RESULT: PASS artifact=$(basename "$ARTIFACT") tables=${COUNT_TABLES} users=${COUNT_USERS} cids=${COUNT_CIDS} bad_uids=${COUNT_BAD_UIDS} dup_cids=${COUNT_DUP_CIDS} duration=${duration}s"
    else
        run_alert "restore drill FAILED: ${RESULT_REASON:-unknown} (artifact $(basename "${ARTIFACT:-?}"))"
        log "RESULT: FAIL ${RESULT_REASON:-unknown}"
        echo "RESULT: FAIL artifact=$(basename "${ARTIFACT:-?}") reason=${RESULT_REASON:-unknown} duration=${duration}s"
    fi
    cleanup_resources
    [ "$result" = "PASS" ] && exit 0
    exit 1
}

cleanup_resources() {
    set +e
    trap - EXIT
    if [ "$DRILL_CREATED" -eq 1 ] && [ -n "$DRILL_NAME" ]; then
        docker rm -f "$DRILL_NAME" >/dev/null 2>&1 || true
    fi
    if [ -n "$WORK_DIR" ] && [ -d "$WORK_DIR" ]; then
        rm -rf -- "$WORK_DIR"
    fi
}

cleanup() {
    local rc=$?
    set +e
    trap - EXIT
    cleanup_resources
    if [ "$rc" -ne 0 ] && [ "$ALERTED" -eq 0 ]; then
        run_alert "restore drill failed unexpectedly (rc=${rc})"
    fi
    exit "$rc"
}
trap cleanup EXIT

RESULT_REASON=""

# ---------------------------------------------------------------------------
# setup
# ---------------------------------------------------------------------------
mkdir -p "$PUBLISHED" "$DRILL_RECORD_DIR" "$LOG_DIR"
chmod 700 "$BACKUP_ROOT" "$PUBLISHED" "$DRILL_RECORD_DIR" "$LOG_DIR" 2>/dev/null || true
touch "$LOG_FILE"
chmod 600 "$LOG_FILE" 2>/dev/null || true
START_EPOCH=$(date +%s)

exec 9>"$LOCK_FILE"
if ! flock -n 9; then
    log "another restore drill is running; skipping"
    exit 0
fi

if ! command -v docker >/dev/null 2>&1; then
    die "docker not installed on this host"
fi

# Resolve the artifact: explicit argument, or newest published artifact.
if [ "$#" -ge 1 ]; then
    ARTIFACT="$1"
else
    newest=$(find "$PUBLISHED" -maxdepth 1 -type f -name 'backup_*.sql.gz' -printf '%f\n' 2>/dev/null | sort | tail -n 1)
    [ -n "$newest" ] || die "no published artifacts in ${PUBLISHED} — run backup-v2.sh first"
    ARTIFACT="$PUBLISHED/$newest"
fi
[ -s "$ARTIFACT" ] || die "artifact not found or empty: $ARTIFACT"

log "restore drill start: artifact=$(basename "$ARTIFACT")"

# Integrity gate before touching Docker (cheap failure first).
if [ -f "${ARTIFACT}.sha256" ] && ! (cd "$(dirname "$ARTIFACT")" && sha256sum -c --status "$(basename "$ARTIFACT").sha256"); then
    die "artifact fails its sha256 checksum"
fi
gzip -t "$ARTIFACT" 2>/dev/null || die "artifact fails gzip -t"
tables_in_dump=$(gzip -dc "$ARTIFACT" 2>/dev/null | grep -c '^CREATE TABLE ' || true)
[ "${tables_in_dump:-0}" -ge "$MIN_TABLES" ] ||
    die "artifact contains ${tables_in_dump:-0} CREATE TABLE blocks (< ${MIN_TABLES})"

# Data-loss gate (mirrors restore-v2.sh verify): refuse cluster-level DDL. A legacy
# `pg_dump --clean --if-exists --create` artifact ends up re-creating its own
# database name inside whatever cluster psql is pointed at. The drill container is
# disposable, but test the artifact the PROD path will actually accept: a drill
# PASS on a legacy artifact would falsely certify it.
cluster_ddl=$(gzip -dc "$ARTIFACT" 2>/dev/null |
    grep -nE '^(DROP DATABASE|CREATE DATABASE|ALTER DATABASE .* OWNER|\\connect)' |
    head -n 3 || true)
if [ -n "$cluster_ddl" ]; then
    log "artifact contains cluster-level DDL (legacy pg_dump --create dump):"
    printf '%s\n' "$cluster_ddl" | tee -a "$LOG_FILE" || true
    die "legacy artifact is NOT accepted (restore-v2 prod would refuse it); convert it first — docs/operations/RESTORE.md 'Legacy artifacts (emergency only)'"
fi
log "artifact has no cluster-level DDL (restore-v2 prod-compatible shape)"

expected_users=""
if [ -f "${ARTIFACT}.manifest.json" ]; then
    expected_users=$(grep -o '"live_users": *[0-9]*' "${ARTIFACT}.manifest.json" 2>/dev/null | grep -o '[0-9]*' | head -n 1 || true)
fi

if ! docker image inspect "$PG_IMAGE" >/dev/null 2>&1; then
    log "pulling ${PG_IMAGE} (one-time)…"
    docker pull "$PG_IMAGE" >>"$LOG_FILE" 2>&1 || die "could not pull ${PG_IMAGE}"
fi

DRILL_NAME="chevalet-drill-$(date -u +%Y%m%d%H%M%S)-$$"
DRILL_PASSWORD=$(head -c 24 /dev/urandom | base64 | tr -d '/+=' | head -c 24)
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/chevalet-drill.XXXXXX")

log "starting disposable ${PG_IMAGE} as ${DRILL_NAME}"
if ! docker run -d --name "$DRILL_NAME" \
    -e POSTGRES_USER=postgres \
    -e POSTGRES_PASSWORD="$DRILL_PASSWORD" \
    -e POSTGRES_DB=mydatabase \
    -p 127.0.0.1::5432 \
    "$PG_IMAGE" >>"$LOG_FILE" 2>&1; then
    die "failed to start the disposable postgres container"
fi
DRILL_CREATED=1
drill_port=$(docker port "$DRILL_NAME" 5432/tcp 2>/dev/null | head -n 1 || true)
log "drill container up (host port: ${drill_port:-n/a}); waiting for readiness"

ready=0
for _ in $(seq 1 60); do
    if docker exec "$DRILL_NAME" pg_isready -U postgres -d mydatabase >/dev/null 2>&1; then
        ready=1
        break
    fi
    sleep 1
done
[ "$ready" -eq 1 ] || die "disposable postgres did not become ready within 60s"

# ---------------------------------------------------------------------------
# restore
# ---------------------------------------------------------------------------
restore_log="$WORK_DIR/restore.log"
log "restoring into the disposable database…"
set +e
gzip -dc "$ARTIFACT" | timeout --signal=TERM "$DRILL_TIMEOUT_SECONDS" \
    docker exec -i "$DRILL_NAME" psql -U postgres -d mydatabase -v ON_ERROR_STOP=1 -q >"$restore_log" 2>&1
restore_rc=$?
set -e
if [ "$restore_rc" -ne 0 ]; then
    log "restore log tail:"; tail -n 15 "$restore_log" | tee -a "$LOG_FILE" || true
    die "psql restore failed (rc=${restore_rc})"
fi
# psql -q still prints serious errors; ON_ERROR_STOP aborts on the first one, so a
# zero exit means a clean restore and the ERROR scan is a belt-and-braces check.
if grep -q '^ERROR:' "$restore_log" 2>/dev/null; then
    log "restore log contains ERROR lines:"; grep -n '^ERROR:' "$restore_log" | head -n 10 | tee -a "$LOG_FILE" || true
    die "restore log contains SQL ERROR lines"
fi

# ---------------------------------------------------------------------------
# invariants
# ---------------------------------------------------------------------------
q() {
    docker exec "$DRILL_NAME" psql -U postgres -d mydatabase -tAc "$1" 2>/dev/null | tr -d '[:space:]'
}

COUNT_TABLES=$(q "select count(*) from information_schema.tables where table_schema='public' and table_type='BASE TABLE'")
COUNT_USERS=$(q "select count(*) from users")
COUNT_CIDS=$(q "select count(*) from cids")
COUNT_BAD_UIDS=$(q "select count(*) from users where uid !~ '^[0-9]+$'")
COUNT_DUP_CIDS=$(q "select coalesce(sum(c - 1), 0) from (select count(*) c from cids group by cid having count(*) > 1) t")

COUNT_TABLES="${COUNT_TABLES:-0}"
COUNT_USERS="${COUNT_USERS:-0}"
COUNT_CIDS="${COUNT_CIDS:-0}"
COUNT_BAD_UIDS="${COUNT_BAD_UIDS:-0}"
COUNT_DUP_CIDS="${COUNT_DUP_CIDS:-0}"

log "counts: tables=${COUNT_TABLES} users=${COUNT_USERS} cids=${COUNT_CIDS} bad_uids=${COUNT_BAD_UIDS} dup_cids=${COUNT_DUP_CIDS}"

[ "$COUNT_TABLES" -ge "$MIN_TABLES" ] || die "restored database has only ${COUNT_TABLES} base tables (< ${MIN_TABLES})"
[ "$COUNT_USERS" -gt 0 ] || die "restored users table is empty"
[ "$COUNT_CIDS" -ge "$COUNT_USERS" ] || die "cids (${COUNT_CIDS}) < users (${COUNT_USERS}); a user is missing its cid"
[ "$COUNT_BAD_UIDS" -eq 0 ] || die "${COUNT_BAD_UIDS} user row(s) have a non-numeric uid"
[ "$COUNT_DUP_CIDS" -eq 0 ] || die "${COUNT_DUP_CIDS} duplicate cid(s)"

if [ -n "$expected_users" ] && [ "$expected_users" -gt 0 ]; then
    diff=$((COUNT_USERS - expected_users))
    [ "$diff" -lt 0 ] && diff=$((-diff))
    allowed=$((expected_users * DRILL_USER_TOLERANCE_PCT / 100))
    [ "$allowed" -lt 25 ] && allowed=25
    [ "$diff" -le "$allowed" ] ||
        die "restored users (${COUNT_USERS}) differs from manifest live_users (${expected_users}) by more than ${allowed}"
    log "manifest cross-check OK: restored users=${COUNT_USERS} vs manifest live_users=${expected_users} (tolerance ${allowed})"
fi

finish PASS
