#!/usr/bin/env bash
# Chevalet watchdog — runs OUTSIDE production (Atur's workstation / any second host).
#
# The 2026-11-23 outage was invisible for ~2.8 hours because nothing watched the
# bot from outside. This script is that watcher: every 5 minutes it SSHes to the
# production host READ-ONLY, collects health facts, and alerts a dedicated ops
# Telegram chat on a state CHANGE (and retries an unchanged failure every few
# hours). It never writes to production.
#
# Checks (read-only):
#   * bot container: running? Docker health status? restart count?
#   * PostgreSQL: `pg_isready` inside the DB container
#   * bot health endpoints: GET /healthz (liveness) and /readyz (DB ping) via a
#     container-local wget — the endpoints bind loopback, so they are NOT reachable
#     from here directly; this is the documented way to probe them
#   * newest verified backup age (< BACKUP_MAX_AGE_MINUTES)
#   * disk usage (< DISK_MAX_PERCENT)
#   * 1-minute load vs nproc (< LOAD_FACTOR_MAX × nproc)
#   * the bot logged "bot polling" since its current start (proof polling is up)
#
# Environment:
#   PROD_HOST=181.41.194.149        (required)
#   PROD_PORT=9011  PROD_USER=root  PROD_KEY=~/.ssh/id_ed25519_vpn
#   BACKUP_DIR=/opt/bots/chevalet/backups-v2/published
#   BOT_CONTAINER=telegram-bot  DB_CONTAINER=telegram-bot-db
#   DB_NAME=mydatabase  DB_USER=root  HEALTH_PORT=12564
#   BACKUP_MAX_AGE_MINUTES=90  DISK_MAX_PERCENT=85  MAX_RESTARTS=10
#   LOAD_FACTOR_MAX=4
#   ALERT_CMD='bash /path/to/ops/alert.sh'   (ALERT_TOKEN/ALERT_CHAT_ID needed)
#   STATE_DIR=${XDG_STATE_HOME:-$HOME/.local/state}/chevalet-watchdog
#   ALERT_REPEAT_MINUTES=360  DIGEST_ENABLED=1
#
# Usage: watchdog.sh [--no-alert]
# Exit:  0 all checks green, 1 at least one check failed (or SSH unreachable).
set -Eeuo pipefail
IFS=$'\n\t'
umask 077

SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

PROD_HOST="${PROD_HOST:-}"
PROD_PORT="${PROD_PORT:-9011}"
PROD_USER="${PROD_USER:-root}"
PROD_KEY="${PROD_KEY:-}"
BACKUP_DIR="${BACKUP_DIR:-/opt/bots/chevalet/backups-v2/published}"
BOT_CONTAINER="${BOT_CONTAINER:-telegram-bot}"
DB_CONTAINER="${DB_CONTAINER:-telegram-bot-db}"
DB_NAME="${DB_NAME:-mydatabase}"
DB_USER="${DB_USER:-root}"
HEALTH_PORT="${HEALTH_PORT:-12564}"
BACKUP_MAX_AGE_MINUTES="${BACKUP_MAX_AGE_MINUTES:-90}"
DISK_MAX_PERCENT="${DISK_MAX_PERCENT:-85}"
MAX_RESTARTS="${MAX_RESTARTS:-10}"
LOAD_FACTOR_MAX="${LOAD_FACTOR_MAX:-4}"
ALERT_REPEAT_MINUTES="${ALERT_REPEAT_MINUTES:-360}"
DIGEST_ENABLED="${DIGEST_ENABLED:-1}"
SSH_STRICT_HOST_KEY_CHECKING="${SSH_STRICT_HOST_KEY_CHECKING:-accept-new}"
USERKNOWN_HOSTS_FILE="${USERKNOWN_HOSTS_FILE:-}"
STATE_DIR="${STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/chevalet-watchdog}"
LOG_FILE="${STATE_DIR}/watchdog.log"
STATE_FILE="${STATE_DIR}/state"
NO_ALERT=0
[ "${1:-}" = "--no-alert" ] && NO_ALERT=1

RUNBOOK="docs/operations/INCIDENT-TRIAGE.md"

log() {
    printf '%s %s\n' "$(date -u +%FT%TZ)" "$*" | tee -a "$LOG_FILE"
}

run_alert() {
    local message="$1"
    if [ "$NO_ALERT" -eq 1 ]; then
        log "ALERT(suppressed): $message"
        return 0
    fi
    if [ -z "${ALERT_CMD:-}" ]; then
        ALERT_CMD="bash ${SELF_DIR}/alert.sh"
    fi
    local -a alert_cmd
    # IFS is globally newline/tab in these scripts; give read an explicit space
    # IFS so an ALERT_CMD like "bash /path/alert.sh" splits into words.
    IFS=' ' read -r -a alert_cmd <<<"$ALERT_CMD"
    log "ALERT: $message"
    "${alert_cmd[@]}" "$message" 2>>"$LOG_FILE" || log "WARN: alert delivery failed (is /etc/chevalet/ops.env configured?)"
    return 0
}

failures=()
add_failure() { failures+=("$1"); }

usage() {
    sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
}
[ "$#" -eq 0 ] || [ "${1:-}" = "--no-alert" ] || usage
[ -n "$PROD_HOST" ] || {
    echo "watchdog: PROD_HOST is required" >&2
    exit 2
}

mkdir -p "$STATE_DIR"
chmod 700 "$STATE_DIR" 2>/dev/null || true
touch "$LOG_FILE"
chmod 600 "$LOG_FILE" 2>/dev/null || true

log "watchdog run: host=${PROD_HOST}:${PROD_PORT} user=${PROD_USER}"

# ---------------------------------------------------------------------------
# collect remote facts (single read-only SSH call; nothing is written remotely)
# ---------------------------------------------------------------------------
printf -v _bk '%q' "$BACKUP_DIR"
printf -v _bot '%q' "$BOT_CONTAINER"
printf -v _dbc '%q' "$DB_CONTAINER"
printf -v _dbn '%q' "$DB_NAME"
printf -v _dbu '%q' "$DB_USER"
printf -v _hp '%q' "$HEALTH_PORT"
remote_env="BACKUP_DIR=${_bk} BOT_CONTAINER=${_bot} DB_CONTAINER=${_dbc} DB_NAME=${_dbn} DB_USER=${_dbu} HEALTH_PORT=${_hp}"

ssh_args=(-F /dev/null -o BatchMode=yes -o "StrictHostKeyChecking=${SSH_STRICT_HOST_KEY_CHECKING}"
    -o ConnectTimeout=8 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -p "$PROD_PORT")
[ -n "$USERKNOWN_HOSTS_FILE" ] && ssh_args+=(-o "UserKnownHostsFile=${USERKNOWN_HOSTS_FILE}")
[ -n "$PROD_KEY" ] && ssh_args+=(-i "$PROD_KEY")

# shellcheck disable=SC2016 # the heredoc is deliberately single-quoted: it runs remotely
remote_script='
set -u
echo "container=$(docker inspect --format "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}} {{.RestartCount}}" "$BOT_CONTAINER" 2>/dev/null || echo missing)"
echo "pg=$( (docker exec "$DB_CONTAINER" pg_isready -U "$DB_USER" -d "$DB_NAME" >/dev/null 2>&1 && echo pg-ok) || echo pg-fail )"
echo "healthz=$( (docker exec "$BOT_CONTAINER" wget -q -O /dev/null "http://127.0.0.1:${HEALTH_PORT}/healthz" >/dev/null 2>&1 && echo http-ok) || echo http-fail )"
echo "readyz=$( (docker exec "$BOT_CONTAINER" wget -q -O /dev/null "http://127.0.0.1:${HEALTH_PORT}/readyz" >/dev/null 2>&1 && echo ready-ok) || echo ready-fail )"
newest=$(ls -1t "$BACKUP_DIR"/backup_*.sql.gz 2>/dev/null | head -n 1 || true)
if [ -n "$newest" ]; then
    echo "backup_age_s=$(( $(date +%s) - $(stat -c %Y "$newest") ))"
else
    echo "backup_age_s=none"
fi
echo "disk_pct=$(df -P / 2>/dev/null | awk "NR==2 {gsub(/%/,\"\",\$5); print \$5}" || echo unknown)"
echo "load1=$(awk "{print \$1}" /proc/loadavg 2>/dev/null || echo unknown)"
echo "nproc=$(nproc 2>/dev/null || echo 0)"
echo "polling=$(docker logs "$BOT_CONTAINER" --since "$(docker inspect --format "{{.State.StartedAt}}" "$BOT_CONTAINER" 2>/dev/null)" 2>&1 | grep -c "bot polling" || true)"
'

set +e
remote_out=$(printf '%s\n' "$remote_script" | timeout 60 ssh "${ssh_args[@]}" "${PROD_USER}@${PROD_HOST}" "env ${remote_env} bash -s" 2>>"$LOG_FILE")
ssh_rc=$?
set -e

container_status=""
container_health=""
container_restarts=""
pg_status=""
healthz_status=""
readyz_status=""
backup_age_s=""
disk_pct=""
load1=""
nproc=""
polling=""

if [ "$ssh_rc" -ne 0 ]; then
    add_failure "host unreachable over SSH (rc=${ssh_rc})"
    log "SSH to ${PROD_HOST}:${PROD_PORT} failed (rc=${ssh_rc})"
else
    while IFS='=' read -r key value; do
        [ -n "$key" ] || continue
        case "$key" in
            container)
                container_status=$(printf '%s' "$value" | awk '{print $1}')
                container_health=$(printf '%s' "$value" | awk '{print $2}')
                container_restarts=$(printf '%s' "$value" | awk '{print $3}')
                ;;
            pg) pg_status="$value" ;;
            healthz) healthz_status="$value" ;;
            readyz) readyz_status="$value" ;;
            backup_age_s) backup_age_s="$value" ;;
            disk_pct) disk_pct="$value" ;;
            load1) load1="$value" ;;
            nproc) nproc="$value" ;;
            polling) polling="$value" ;;
            *) : ;;
        esac
    done <<<"$remote_out"

    [ -n "$container_status" ] || add_failure "could not read the bot container state (docker missing?)"
    if [ "$container_status" = "running" ]; then
        case "$container_health" in
            healthy) : ;;
            starting) log "note: bot health is 'starting' (deploy/restart in progress?)" ;;
            none) add_failure "bot container has no healthcheck configured" ;;
            *) add_failure "bot container health=${container_health:-unknown}" ;;
        esac
    else
        add_failure "bot container state=${container_status:-missing}"
    fi
    if [ -n "$container_restarts" ] && [ "$container_restarts" -gt "$MAX_RESTARTS" ]; then
        add_failure "bot restart count=${container_restarts} (> ${MAX_RESTARTS})"
    fi
    [ "$pg_status" = "pg-ok" ] || add_failure "PostgreSQL not ready (pg_isready failed)"
    [ "$healthz_status" = "http-ok" ] || add_failure "/healthz failed (bot not live)"
    [ "$readyz_status" = "ready-ok" ] || add_failure "/readyz failed (DB/assets/backups not ready)"
    case "$backup_age_s" in
        none | "" | *[!0-9]*) add_failure "no verified backup artifact found in ${BACKUP_DIR}" ;;
        *)
            if [ "$backup_age_s" -gt $((BACKUP_MAX_AGE_MINUTES * 60)) ]; then
                add_failure "newest backup is $((backup_age_s / 60)) min old (> ${BACKUP_MAX_AGE_MINUTES} min)"
            fi
            ;;
    esac
    case "$disk_pct" in
        "" | unknown | *[!0-9]*) log "WARN: could not read disk usage" ;;
        *) [ "$disk_pct" -lt "$DISK_MAX_PERCENT" ] || add_failure "disk usage ${disk_pct}% (>= ${DISK_MAX_PERCENT}%)" ;;
    esac
    if [ "$load1" != "unknown" ] && [ -n "$nproc" ] && [ "$nproc" -gt 0 ]; then
        if awk -v l="$load1" -v n="$nproc" -v f="$LOAD_FACTOR_MAX" 'BEGIN { exit !(l > n * f) }'; then
            add_failure "load1=${load1} exceeds ${LOAD_FACTOR_MAX}x nproc (${nproc})"
        fi
    fi
    case "$polling" in
        "" | *[!0-9]*) add_failure "could not count 'bot polling' log lines" ;;
        *) [ "$polling" -gt 0 ] || add_failure "bot has not logged 'bot polling' since this start" ;;
    esac
fi

# ---------------------------------------------------------------------------
# state-change alerting (never alert the same unchanged state every 5 minutes)
# ---------------------------------------------------------------------------
if [ "${#failures[@]}" -eq 0 ]; then
    new_state="OK"
else
    joined=$(printf '%s; ' "${failures[@]}")
    new_state="FAIL: ${joined%; }"
fi

prev_state=""
prev_alert_epoch=0
prev_digest_date=""
if [ -f "$STATE_FILE" ]; then
    # shellcheck disable=SC1090,SC1091 # simple key=value state file written below
    . "$STATE_FILE"
    prev_state="${last_state:-}"
    prev_alert_epoch="${last_alert_epoch:-0}"
    prev_digest_date="${last_digest_date:-}"
fi

now=$(date +%s)
today=$(date -u +%Y-%m-%d)
alerted=0

if [ "$new_state" != "$prev_state" ]; then
    if [ "$new_state" = "OK" ]; then
        run_alert "WATCHDOG RECOVERED: all checks green again on ${PROD_HOST}. Previous: ${prev_state:-unknown}. Runbook: ${RUNBOOK}"
    else
        run_alert "WATCHDOG FAILURE on ${PROD_HOST}: ${new_state}. Runbook: ${RUNBOOK}"
    fi
    alerted=1
elif [ "$new_state" != "OK" ] && [ $((now - prev_alert_epoch)) -ge $((ALERT_REPEAT_MINUTES * 60)) ]; then
    run_alert "WATCHDOG STILL FAILING on ${PROD_HOST} (reminder): ${new_state}. Runbook: ${RUNBOOK}"
    alerted=1
elif [ "$new_state" = "OK" ] && [ "$DIGEST_ENABLED" -eq 1 ] && [ "$prev_digest_date" != "$today" ]; then
    run_alert "WATCHDOG daily all-green on ${PROD_HOST}: bot healthy, DB ready, backup $((backup_age_s / 60)) min old, disk ${disk_pct}%, load ${load1}/${nproc}. Runbook: ${RUNBOOK}"
    alerted=1
fi

if [ "$alerted" -eq 1 ]; then
    prev_alert_epoch=$now
fi
{
    printf 'last_state=%q\n' "$new_state"
    printf 'last_alert_epoch=%q\n' "$prev_alert_epoch"
    if [ "$new_state" = "OK" ] && [ "$prev_digest_date" != "$today" ]; then
        printf 'last_digest_date=%q\n' "$today"
    else
        printf 'last_digest_date=%q\n' "$prev_digest_date"
    fi
} >"$STATE_FILE"
chmod 600 "$STATE_FILE" 2>/dev/null || true

if [ "$new_state" = "OK" ]; then
    log "watchdog OK: backup_age=$((backup_age_s / 60))m disk=${disk_pct}% load=${load1}/${nproc} restarts=${container_restarts}"
    exit 0
fi
log "watchdog FAIL: ${new_state}"
echo "WATCHDOG FAIL: ${new_state}"
exit 1
