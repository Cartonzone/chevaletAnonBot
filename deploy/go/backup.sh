#!/bin/sh
# HISTORICAL / STAGING ONLY — the old-host hourly backup (Go-port cutover era).
#
# Do NOT use this for current production: it targets the retired
# /opt/chevalet-go-staging layout and has none of the v2 guarantees (structure
# checks, sha256 manifest, off-host push, drill, alerting). Production uses
# deploy/prod/ops/backup-v2.sh instead — see deploy/prod/README.md.
#
# What it did: dump telegram-bot-db with the postgres container's OWN credentials
# into the host dir bind-mounted to the bot's /app/backups (so `/admin backup`
# could send the newest file). Kept the newest $KEEP backups.
#
# NOTE: no `docker exec -t` here — cron has no TTY and -t both fails and corrupts
# the piped binary (that was the old backup-tiered.sh bug).
#
# Cron on the OLD host (hourly):
#   0 * * * * /opt/chevalet-go-staging/deploy/go/backup.sh >> /opt/chevalet-go-staging/logs/backup.log 2>&1
set -eu

BACKUP_DIR="${BACKUP_DIR:-/opt/chevalet-go-staging/backups}"
DB_CONTAINER="${DB_CONTAINER:-telegram-bot-db}"
DB_NAME="${DB_NAME:-mydatabase}"
KEEP="${KEEP:-100}"

mkdir -p "$BACKUP_DIR"
TS=$(date +%Y%m%d_%H%M%S)
OUT="$BACKUP_DIR/backup_${DB_NAME}_${TS}.sql.gz"

docker exec "$DB_CONTAINER" sh -c \
  'PGPASSWORD=$POSTGRES_PASSWORD pg_dump -U $POSTGRES_USER -d $POSTGRES_DB --clean --if-exists --create --encoding=UTF8' \
  | gzip > "$OUT"

# Guard against a truncated/empty dump being kept as a "backup".
SIZE=$(stat -c%s "$OUT" 2>/dev/null || echo 0)
if [ "$SIZE" -lt 1000 ]; then
  echo "$(date -u +%FT%TZ) ERROR: backup too small ($SIZE bytes), removing $OUT"
  rm -f "$OUT"
  exit 1
fi

# Retention: keep the newest $KEEP backups.
ls -1t "$BACKUP_DIR"/backup_*.sql.gz 2>/dev/null | tail -n +$((KEEP + 1)) | xargs -r rm -f

echo "$(date -u +%FT%TZ) backup ok: $OUT ($(du -h "$OUT" | cut -f1))"
