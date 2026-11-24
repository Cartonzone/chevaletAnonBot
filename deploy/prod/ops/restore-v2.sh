#!/usr/bin/env bash
# Chevalet restore v2 — verify | drill | prod (safe, rehearsable).
#
# The 2026-11-23 incident proved the old restore.sh was unsafe for production: it
# restored in place over the live database with no pre-restore dump, no
# verification and stale container names. This script has three modes:
#
#   verify <artifact>   Integrity only. No database is touched. Checks sha256
#                       sidecar, gzip integrity, pg_dump completion footer,
#                       CREATE TABLE count and manifest consistency, and REFUSES
#                       artifacts containing cluster-level DDL (legacy
#                       `pg_dump --create` dumps: DROP/CREATE DATABASE, \connect)
#                       because the restore pipeline feeds the dump to psql -d
#                       <target>: such a line would act on the whole cluster.
#   drill <artifact>    Delegates to restore-drill.sh: restores into a throwaway
#                       local postgres container and asserts the data invariants.
#                       Safe to run any time; this is what the monthly timer runs.
#   prod <artifact> --approve [--yes]
#                       The real thing, and it refuses to run half-prepared:
#                         - requires --approve and, interactively, typing the DB
#                           name to confirm (`--yes` is only for a scripted run
#                           after --approve);
#                         - accepts ONLY chevalet-backup-v2 artifacts: verification
#                           refuses cluster-level DDL AND prod requires the v2
#                           manifest sidecar (legacy dumps are emergency-only and
#                           must be converted first — docs/operations/RESTORE.md);
#                         - takes a FRESH pre-restore dump with backup-v2.sh and
#                           aborts if it does not pass verification;
#                         - stops ONLY the bot container (never `compose down`,
#                           never the database);
#                         - restores into a NEW database <db>_restore_<ts>, verifies
#                           the invariants there, then atomically RENAMEs
#                           live -> <db>_pre_restore_<ts> and new -> live (the old
#                           database is kept for rollback until you choose to drop
#                           it);
#                         - starts the bot and waits for /healthz + "bot polling";
#                         - prints the exact manual rollback commands.
#
# Environment (defaults are the production values):
#   DB_CONTAINER=telegram-bot-db   BOT_CONTAINER=telegram-bot
#   DB_NAME=mydatabase             DB_USER=root
#   BACKUP_ROOT=/opt/bots/chevalet/backups-v2
#   OPS_DIR=<dir of this script>   ALERT_CMD='bash …/ops/alert.sh'
#   HEALTH_PORT=12564              BOT_START_TIMEOUT_SECONDS=180
#   MIN_TABLES=8                   PROD_USER_TOLERANCE_PCT=2
#
# Exit codes: 0 success, 1 failure, 2 usage, 3 refused (safety gate).
set -Eeuo pipefail
IFS=$'\n\t'
umask 077

SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
OPS_DIR="${OPS_DIR:-$SELF_DIR}"
DB_CONTAINER="${DB_CONTAINER:-telegram-bot-db}"
BOT_CONTAINER="${BOT_CONTAINER:-telegram-bot}"
DB_NAME="${DB_NAME:-mydatabase}"
DB_USER="${DB_USER:-root}"
BACKUP_ROOT="${BACKUP_ROOT:-/opt/bots/chevalet/backups-v2}"
PUBLISHED="$BACKUP_ROOT/published"
RESTORE_RECORD_DIR="${RESTORE_RECORD_DIR:-$BACKUP_ROOT/restores}"
LOG_DIR="$BACKUP_ROOT/log"
LOG_FILE="$LOG_DIR/restore.log"
ALERT_CMD="${ALERT_CMD:-bash ${SELF_DIR}/alert.sh}"
HEALTH_PORT="${HEALTH_PORT:-12564}"
BOT_START_TIMEOUT_SECONDS="${BOT_START_TIMEOUT_SECONDS:-180}"
MIN_TABLES="${MIN_TABLES:-8}"
PROD_USER_TOLERANCE_PCT="${PROD_USER_TOLERANCE_PCT:-2}"
RESTORE_TIMEOUT_SECONDS="${RESTORE_TIMEOUT_SECONDS:-1800}"

MODE=""
ARTIFACT=""
APPROVE=0
ASSUME_YES=0
START_EPOCH=$(date +%s)
BOT_STOPPED=0
SWAP_DONE=0
NEW_DB=""
OLD_DB=""
SAFETY_ARTIFACT=""

log() {
    printf '%s %s\n' "$(date -u +%FT%TZ)" "$*" | tee -a "$LOG_FILE"
}

run_alert() {
    local message="$1"
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
    local code="${2:-1}"
    log "ERROR: $1"
    run_alert "$1"
    exit "$code"
}

usage() {
    sed -n '2,50p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
}

mkdir -p "$PUBLISHED" "$RESTORE_RECORD_DIR" "$LOG_DIR" 2>/dev/null || true
chmod 700 "$BACKUP_ROOT" "$PUBLISHED" "$RESTORE_RECORD_DIR" "$LOG_DIR" 2>/dev/null || true
touch "$LOG_FILE" 2>/dev/null || true
chmod 600 "$LOG_FILE" 2>/dev/null || true

# ---------------------------------------------------------------------------
# argument parsing
# ---------------------------------------------------------------------------
while [ "$#" -gt 0 ]; do
    case "$1" in
        verify | drill | prod)
            [ -z "$MODE" ] || die "only one mode may be given" 2
            MODE="$1"
            ;;
        --approve) APPROVE=1 ;;
        --yes) ASSUME_YES=1 ;;
        -h | --help) usage ;;
        -*) die "unknown option: $1" 2 ;;
        *)
            [ -z "$ARTIFACT" ] || die "only one artifact may be given" 2
            ARTIFACT="$1"
            ;;
    esac
    shift
done
[ -n "$MODE" ] || usage

validate_identifier() {
    case "$2" in
        "" | *[!A-Za-z0-9_]*)
            die "${1} must be a plain identifier, got: ${2}" 2
            ;;
    esac
}
validate_identifier DB_NAME "$DB_NAME"
validate_identifier DB_USER "$DB_USER"

resolve_artifact() {
    if [ -n "$ARTIFACT" ]; then
        [ -s "$ARTIFACT" ] || die "artifact not found or empty: $ARTIFACT"
        ARTIFACT=$(cd "$(dirname "$ARTIFACT")" && pwd)/$(basename "$ARTIFACT")
        return 0
    fi
    local newest
    newest=$(find "$PUBLISHED" -maxdepth 1 -type f -name 'backup_*.sql.gz' -printf '%f\n' 2>/dev/null | sort | tail -n 1)
    [ -n "$newest" ] || die "no published artifacts in ${PUBLISHED}"
    ARTIFACT="$PUBLISHED/$newest"
}

# ---------------------------------------------------------------------------
# integrity verification (mode: verify; also the gate for drill/prod)
# ---------------------------------------------------------------------------
verify_artifact_integrity() {
    local artifact="$1" base size
    base=$(basename "$artifact")
    [ -s "$artifact" ] || {
        log "verify: FAIL missing/empty artifact: $artifact"
        return 1
    }
    if [ -s "${artifact}.sha256" ]; then
        (cd "$(dirname "$artifact")" && sha256sum -c --status "${base}.sha256") || {
            log "verify: FAIL sha256 mismatch ($base)"
            return 1
        }
        log "verify: sha256 OK ($base)"
    else
        log "verify: WARN no ${base}.sha256 sidecar — checksum not independently verified"
    fi
    gzip -t "$artifact" 2>/dev/null || {
        log "verify: FAIL gzip -t ($base)"
        return 1
    }
    log "verify: gzip integrity OK ($base)"
    footer_count=$(gzip -dc "$artifact" 2>/dev/null | grep -cF -- '-- PostgreSQL database dump complete' || true)
    [ "${footer_count:-0}" -ge 1 ] || {
        log "verify: FAIL pg_dump completion footer missing ($base)"
        return 1
    }
    tables=$(gzip -dc "$artifact" 2>/dev/null | grep -c '^CREATE TABLE ' || true)
    [ "${tables:-0}" -ge "$MIN_TABLES" ] || {
        log "verify: FAIL only ${tables:-0} CREATE TABLE blocks (< ${MIN_TABLES}) ($base)"
        return 1
    }
    log "verify: pg_dump structure OK (${tables} tables, footer present)"

    # DATA-LOSS GATE (v3.4.1): refuse cluster-level DDL. The legacy dump scripts
    # (root backup.sh, deploy/go/backup.sh) used `pg_dump --clean --if-exists
    # --create`, so their artifacts contain `DROP DATABASE <live>; CREATE DATABASE
    # <live>; \connect <live>`. This pipeline feeds the dump to `psql -d <target>`:
    # with psql connected to a DIFFERENT database (restore-v2 prod restores into
    # <db>_restore_<ts>), the DROP DATABASE line executes against the WHOLE
    # cluster — while the bot is stopped that drops the LIVE database before any
    # invariant check runs. Reproduced locally: psql exited 0 with zero ERROR
    # lines and the live DB was re-created from the artifact. Reject such
    # artifacts in EVERY mode (verify/drill/prod); they are only restorable inside
    # a throwaway isolated container (see docs/operations/RESTORE.md).
    local cluster_ddl
    cluster_ddl=$(gzip -dc "$artifact" 2>/dev/null |
        grep -nE '^(DROP DATABASE|CREATE DATABASE|ALTER DATABASE .* OWNER|\\connect)' |
        head -n 3 || true)
    if [ -n "$cluster_ddl" ]; then
        log "verify: FAIL artifact contains cluster-level DDL (legacy pg_dump --create dump) — unsafe to feed to psql:"
        printf '%s\n' "$cluster_ddl" | while IFS= read -r ddl_line; do
            log "verify:   ${ddl_line}"
        done
        log "verify: REFUSED — only chevalet-backup-v2 artifacts are accepted for restore (legacy dump?)
            see docs/operations/RESTORE.md 'Legacy artifacts (emergency only)' for the safe conversion procedure"
        return 1
    fi
    log "verify: no cluster-level DDL (safe to restore into a target database)"
    size=$(stat -c %s "$artifact")
    log "verify: size ${size} bytes"
    if [ -s "${artifact}.manifest.json" ]; then
        if ! grep -q '"format": *"chevalet-backup-v2"' "${artifact}.manifest.json"; then
            log "verify: WARN manifest is not a chevalet-backup-v2 manifest (legacy artifact?)"
        else
            local manifest_sha manifest_size
            manifest_sha=$(grep -o '"sha256": *"[0-9a-f]*"' "${artifact}.manifest.json" | grep -o '[0-9a-f]\{64\}' | head -n 1 || true)
            manifest_size=$(grep -o '"size_bytes": *[0-9]*' "${artifact}.manifest.json" | grep -o '[0-9]*' | head -n 1 || true)
            if [ -n "$manifest_sha" ] && [ "$manifest_sha" != "$(sha256sum "$artifact" | cut -d' ' -f1)" ]; then
                log "verify: FAIL manifest sha256 disagrees with the artifact"
                return 1
            fi
            if [ -n "$manifest_size" ] && [ "$manifest_size" != "$size" ]; then
                log "verify: FAIL manifest size_bytes (${manifest_size}) disagrees with the artifact (${size})"
                return 1
            fi
            log "verify: manifest agrees (sha256 + size)"
        fi
    fi
    return 0
}

# ---------------------------------------------------------------------------
# mode: verify / drill
# ---------------------------------------------------------------------------
resolve_artifact
case "$MODE" in
    verify)
        if ! verify_artifact_integrity "$ARTIFACT"; then
            die "artifact FAILED verification: $(basename "$ARTIFACT")"
        fi
        log "verify: PASS $(basename "$ARTIFACT")"
        echo "RESULT: VERIFY PASS artifact=$(basename "$ARTIFACT")"
        exit 0
        ;;
    drill)
        verify_artifact_integrity "$ARTIFACT" || die "artifact FAILED verification: $(basename "$ARTIFACT")"
        log "drill: delegating to ${OPS_DIR}/restore-drill.sh"
        exec bash "${OPS_DIR}/restore-drill.sh" "$ARTIFACT"
        ;;
    prod) ;;
    *) usage ;;
esac

# ---------------------------------------------------------------------------
# mode: prod — safety gates
# ---------------------------------------------------------------------------
if [ "$APPROVE" -ne 1 ]; then
    die "prod restore refused: --approve is required (this replaces the live database)" 3
fi
if [ ! -t 0 ] && [ "$ASSUME_YES" -ne 1 ]; then
    die "prod restore refused: no TTY for interactive confirmation; pass --yes for a scripted run (still requires --approve)" 3
fi
command -v docker >/dev/null 2>&1 || die "docker is not installed on this host"

log "=== PROD RESTORE REQUESTED: $(basename "$ARTIFACT") ==="
verify_artifact_integrity "$ARTIFACT" || die "prod restore refused: artifact failed verification" 3

# PROD ONLY: require a chevalet-backup-v2 manifest sidecar. backup-v2.sh writes it
# atomically next to the artifact and it records format/sha256/size/live_users, so a
# legacy (pre-v2) dump can never satisfy this. Defense-in-depth behind the
# cluster-DDL gate above: prod restores must only ever use artifacts this release
# produced. The documented legacy-artifact emergency path converts a legacy dump
# into a v2-shaped artifact first (docs/operations/RESTORE.md).
if [ ! -s "${ARTIFACT}.manifest.json" ]; then
    die "prod restore refused: missing $(basename "$ARTIFACT").manifest.json sidecar — only chevalet-backup-v2 artifacts are accepted (docs/operations/RESTORE.md: legacy artifacts are emergency-only and must be converted first)" 3
fi
if ! grep -q '"format": *"chevalet-backup-v2"' "${ARTIFACT}.manifest.json"; then
    die "prod restore refused: manifest is not a chevalet-backup-v2 manifest — only v2 artifacts are accepted for prod (docs/operations/RESTORE.md: legacy artifacts are emergency-only and must be converted first)" 3
fi

if [ -t 0 ]; then
    echo
    echo "This will REPLACE the live database '${DB_NAME}' on container '${DB_CONTAINER}'"
    echo "with artifact: $(basename "$ARTIFACT")"
    echo "The bot container '${BOT_CONTAINER}' will be stopped for the duration."
    echo "A fresh pre-restore dump will be taken first; the old database will be kept"
    echo "as '${DB_NAME}_pre_restore_<ts>' for rollback."
    echo
    printf 'Type the database name (%s) to confirm: ' "$DB_NAME"
    read -r confirmation
    [ "$confirmation" = "$DB_NAME" ] || die "prod restore refused: confirmation did not match" 3
fi

# Pre-restore safety dump — never restore without one, even if the DB looks broken.
log "prod: taking a fresh pre-restore dump with ${OPS_DIR}/backup-v2.sh"
if ! BACKUP_ROOT="$BACKUP_ROOT" DB_CONTAINER="$DB_CONTAINER" DB_NAME="$DB_NAME" DB_USER="$DB_USER" \
    bash "${OPS_DIR}/backup-v2.sh"; then
    die "prod restore aborted: pre-restore backup FAILED (bot untouched, database untouched)"
fi
SAFETY_ARTIFACT=$(find "$PUBLISHED" -maxdepth 1 -type f -name 'backup_*.sql.gz' -printf '%f\n' 2>/dev/null | sort | tail -n 1)
[ -n "$SAFETY_ARTIFACT" ] || die "prod restore aborted: pre-restore backup produced no artifact"
SAFETY_ARTIFACT="$PUBLISHED/$SAFETY_ARTIFACT"
if [ "$(stat -c %Y "$SAFETY_ARTIFACT")" -lt $((START_EPOCH - 120)) ]; then
    die "prod restore aborted: pre-restore artifact $(basename "$SAFETY_ARTIFACT") is not fresh"
fi
log "prod: pre-restore backup OK: $(basename "$SAFETY_ARTIFACT")"

stamp=$(date -u +%Y%m%dT%H%M%SZ)
old_db="${DB_NAME}_pre_restore_${stamp}"
new_db="${DB_NAME}_restore_${stamp}"
if [ "${#old_db}" -gt 63 ] || [ "${#new_db}" -gt 63 ]; then
    die "prod restore aborted: generated database name exceeds PostgreSQL's 63-byte limit"
fi

# Restart the bot on abort for anything that happens before the database swap.
prod_cleanup() {
    local rc=$?
    set +e
    trap - EXIT
    if [ "$rc" -ne 0 ]; then
        if [ "$BOT_STOPPED" -eq 1 ] && [ "$SWAP_DONE" -eq 0 ]; then
            log "prod: aborting before the database swap — restarting the bot on the current (untouched) database"
            docker start "$BOT_CONTAINER" >/dev/null 2>&1 || log "ERROR: could not restart ${BOT_CONTAINER}; start it manually"
        fi
        if [ -n "$NEW_DB" ] && [ "$SWAP_DONE" -eq 0 ]; then
            log "note: the restored staging database ${NEW_DB} was left in place for inspection; drop it manually if unneeded"
        fi
    fi
    exit "$rc"
}
trap prod_cleanup EXIT

# ---------------------------------------------------------------------------
# mode: prod — stop the BOT only (never the database, never compose down)
# ---------------------------------------------------------------------------
log "prod: stopping bot container ${BOT_CONTAINER} (database container is NOT touched)"
docker stop -t 70 "$BOT_CONTAINER" >>"$LOG_FILE" 2>&1 || die "failed to stop ${BOT_CONTAINER}"
BOT_STOPPED=1
if [ "$(docker inspect --format '{{.State.Running}}' "$BOT_CONTAINER" 2>/dev/null || echo false)" != "false" ]; then
    die "${BOT_CONTAINER} is still running after docker stop — aborting"
fi
log "prod: bot stopped"

psql_run() {
    # $1 = database, rest = SQL
    local database="$1"
    shift
    docker exec -i "$DB_CONTAINER" psql -U "$DB_USER" -d "$database" -v ON_ERROR_STOP=1 -q "$@"
}

psql_query() {
    local database="$1" sql="$2"
    docker exec "$DB_CONTAINER" psql -U "$DB_USER" -d "$database" -tAc "$sql" 2>/dev/null | tr -d '[:space:]'
}

# ---------------------------------------------------------------------------
# mode: prod — create + restore into the NEW database
# ---------------------------------------------------------------------------
NEW_DB="$new_db"
log "prod: creating database ${NEW_DB}"
psql_run postgres -c "CREATE DATABASE \"${NEW_DB}\" TEMPLATE template0 ENCODING 'UTF8'" >>"$LOG_FILE" 2>&1 ||
    die "failed to create database ${NEW_DB}"

restore_log="$RESTORE_RECORD_DIR/restore-${stamp}.psql.log"
log "prod: restoring $(basename "$ARTIFACT") into ${NEW_DB} (must be error-free)"
set +e
gzip -dc "$ARTIFACT" | timeout --signal=TERM "$RESTORE_TIMEOUT_SECONDS" \
    docker exec -i "$DB_CONTAINER" psql -U "$DB_USER" -d "$NEW_DB" -v ON_ERROR_STOP=1 -q >"$restore_log" 2>&1
restore_rc=$?
set -e
chmod 600 "$restore_log" 2>/dev/null || true
if [ "$restore_rc" -ne 0 ]; then
    tail -n 20 "$restore_log" | tee -a "$LOG_FILE" || true
    die "restore into ${NEW_DB} failed (rc=${restore_rc}); live database untouched"
fi
if grep -q '^ERROR:' "$restore_log" 2>/dev/null; then
    grep -n '^ERROR:' "$restore_log" | head -n 10 | tee -a "$LOG_FILE" || true
    die "restore into ${NEW_DB} produced SQL ERROR lines; live database untouched"
fi

log "prod: verifying invariants in ${NEW_DB}"
C_TABLES=$(psql_query "$NEW_DB" "select count(*) from information_schema.tables where table_schema='public' and table_type='BASE TABLE'")
C_USERS=$(psql_query "$NEW_DB" "select count(*) from users")
C_CIDS=$(psql_query "$NEW_DB" "select count(*) from cids")
C_BADUIDS=$(psql_query "$NEW_DB" "select count(*) from users where uid !~ '^[0-9]+$'")
C_DUPCIDS=$(psql_query "$NEW_DB" "select coalesce(sum(c - 1), 0) from (select count(*) c from cids group by cid having count(*) > 1) t")
C_TABLES="${C_TABLES:-0}"
C_USERS="${C_USERS:-0}"
C_CIDS="${C_CIDS:-0}"
C_BADUIDS="${C_BADUIDS:-0}"
C_DUPCIDS="${C_DUPCIDS:-0}"
log "prod: new-db counts: tables=${C_TABLES} users=${C_USERS} cids=${C_CIDS} bad_uids=${C_BADUIDS} dup_cids=${C_DUPCIDS}"
[ "$C_TABLES" -ge "$MIN_TABLES" ] || die "new database has only ${C_TABLES} tables (< ${MIN_TABLES}); live database untouched"
[ "$C_USERS" -gt 0 ] || die "new database has no users; live database untouched"
[ "$C_CIDS" -ge "$C_USERS" ] || die "new database cids (${C_CIDS}) < users (${C_USERS}); live database untouched"
[ "$C_BADUIDS" -eq 0 ] || die "new database has ${C_BADUIDS} non-numeric uids; live database untouched"
[ "$C_DUPCIDS" -eq 0 ] || die "new database has ${C_DUPCIDS} duplicate cids; live database untouched"

expected_users=""
if [ -s "${ARTIFACT}.manifest.json" ]; then
    expected_users=$(grep -o '"live_users": *[0-9]*' "${ARTIFACT}.manifest.json" | grep -o '[0-9]*' | head -n 1 || true)
fi
if [ -n "$expected_users" ] && [ "$expected_users" -gt 0 ]; then
    diff=$((C_USERS - expected_users))
    [ "$diff" -lt 0 ] && diff=$((-diff))
    allowed=$((expected_users * PROD_USER_TOLERANCE_PCT / 100))
    [ "$allowed" -lt 20 ] && allowed=20
    [ "$diff" -le "$allowed" ] || die "new database users (${C_USERS}) differ from manifest (${expected_users}) by more than ${allowed}; live database untouched"
    log "prod: manifest cross-check OK (users ${C_USERS} vs ${expected_users})"
fi

# ---------------------------------------------------------------------------
# mode: prod — atomic swap
# ---------------------------------------------------------------------------
OLD_DB="$old_db"
live_sessions=$(psql_query postgres "select count(*) from pg_stat_activity where datname = '${DB_NAME}' and pid <> pg_backend_pid()")
if [ "${live_sessions:-0}" -ne 0 ]; then
    log "prod: terminating ${live_sessions} remaining connection(s) to ${DB_NAME}"
    psql_run postgres -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '${DB_NAME}' AND pid <> pg_backend_pid()" >>"$LOG_FILE" 2>&1 || true
fi

log "prod: swapping databases: ${DB_NAME} -> ${OLD_DB}; ${NEW_DB} -> ${DB_NAME}"
if ! psql_run postgres -c "ALTER DATABASE \"${DB_NAME}\" RENAME TO \"${OLD_DB}\"" >>"$LOG_FILE" 2>&1; then
    die "failed to rename the live database; it is still the live one (verify with psql \\l). Bot will be restarted."
fi
if ! psql_run postgres -c "ALTER DATABASE \"${NEW_DB}\" RENAME TO \"${DB_NAME}\"" >>"$LOG_FILE" 2>&1; then
    log "CRITICAL: second rename failed; trying to rename ${OLD_DB} back to ${DB_NAME}"
    psql_run postgres -c "ALTER DATABASE \"${OLD_DB}\" RENAME TO \"${DB_NAME}\"" >>"$LOG_FILE" 2>&1 ||
        log "CRITICAL: rename-back failed — the database is named ${OLD_DB}; fix manually NOW"
    die "database swap failed; manual attention required"
fi
SWAP_DONE=1
NEW_DB="" # no longer exists under the staging name
log "prod: swap complete; live database is now the restored copy"

# ---------------------------------------------------------------------------
# mode: prod — start the bot and verify it is actually live
# ---------------------------------------------------------------------------
log "prod: starting bot container ${BOT_CONTAINER}"
docker start "$BOT_CONTAINER" >>"$LOG_FILE" 2>&1 || die "database swapped but the bot failed to start — see rollback below"
BOT_STOPPED=0
started_at=$(docker inspect --format '{{.State.StartedAt}}' "$BOT_CONTAINER" 2>/dev/null || true)

healthy=0
for _ in $(seq 1 "$BOT_START_TIMEOUT_SECONDS"); do
    state=$(docker inspect --format '{{.State.Health.Status}}' "$BOT_CONTAINER" 2>/dev/null || echo missing)
    if [ "$state" = "healthy" ]; then
        healthy=1
        break
    fi
    sleep 1
done
if [ "$healthy" -ne 1 ]; then
    log "bot did not report healthy within ${BOT_START_TIMEOUT_SECONDS}s; last 30 log lines:"
    docker logs "$BOT_CONTAINER" --tail 30 >>"$LOG_FILE" 2>&1 || true
    die "database swapped but the bot is not healthy — run the rollback commands below" 1
fi

polling=0
for _ in $(seq 1 120); do
    if docker logs "$BOT_CONTAINER" --since "$started_at" 2>&1 | grep -q 'bot polling'; then
        polling=1
        break
    fi
    sleep 1
done
[ "$polling" -eq 1 ] || die "bot is healthy but never logged 'bot polling' — check Telegram/PROXY; rollback commands below"

if ! docker exec "$BOT_CONTAINER" wget -q -O /dev/null "http://127.0.0.1:${HEALTH_PORT}/healthz" >/dev/null 2>&1; then
    log "WARN: /healthz probe over HTTP failed even though the container is healthy"
fi

duration=$(($(date +%s) - START_EPOCH))
record="$RESTORE_RECORD_DIR/restore-${stamp}.json"
cat >"$record" <<EOF
{
  "format": "chevalet-restore-v2",
  "created_utc": "$(date -u +%FT%TZ)",
  "host": "$(hostname -s)",
  "result": "PASS",
  "artifact": "$(basename "$ARTIFACT")",
  "artifact_sha256": "$(sha256sum "$ARTIFACT" | cut -d' ' -f1)",
  "pre_restore_artifact": "$(basename "$SAFETY_ARTIFACT")",
  "new_live_database": "${DB_NAME}",
  "previous_database_kept_as": "${OLD_DB}",
  "tables": ${C_TABLES},
  "users": ${C_USERS},
  "cids": ${C_CIDS},
  "bad_uids": ${C_BADUIDS},
  "dup_cids": ${C_DUPCIDS},
  "duration_seconds": ${duration}
}
EOF
chmod 600 "$record" 2>/dev/null || true
trap - EXIT

log "RESULT: PASS prod restore users=${C_USERS} cids=${C_CIDS} duration=${duration}s (previous DB kept as ${OLD_DB})"
echo
echo "RESULT: PASS prod-restore artifact=$(basename "$ARTIFACT") users=${C_USERS} cids=${C_CIDS} duration=${duration}s"
echo "Previous database kept as: ${OLD_DB} (drop it manually once you are satisfied)."
echo
echo "ROLLBACK (only if the new data must be reverted) — exact commands:"
cat <<EOF
  docker stop ${BOT_CONTAINER}
  docker exec ${DB_CONTAINER} psql -U ${DB_USER} -d postgres -v ON_ERROR_STOP=1 \\
    -c 'ALTER DATABASE "${DB_NAME}" RENAME TO "${DB_NAME}_restore_FAILED_${stamp}";' \\
    -c 'ALTER DATABASE "${OLD_DB}" RENAME TO "${DB_NAME}";'
  docker start ${BOT_CONTAINER}
EOF
echo
exit 0
