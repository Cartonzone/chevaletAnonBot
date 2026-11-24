# RB-05 — Disk full / storage I/O stall (the 2026-11-23 class)

The old host died when its storage layer stalled (I/O blocked >362 s, load spike,
PostgreSQL timeouts, provider deleted the VM). The current host: 25 GB disk, single
virtio disk, no SMART, 72 % used at audit time. The bot itself is tiny; co-tenant
data and Docker artifacts are the usual consumers.

## 1. Detect (watchdog alerts, or these read-only checks)

```sh
df -h /; df -i /; du -xhd1 / 2>/dev/null | sort -h | tail -15
docker system df
docker image ls --format '{{.Repository}}:{{.Tag}}\t{{.Size}}' | sort -k2 -h | tail
journalctl --since '-1 h' -p warning --no-pager | grep -iE 'no space|i/o error|blocked' | tail
dmesg -T | grep -iE 'i/o error|blocked for more than|journal|ext4|xfs' | tail
cat /proc/loadavg; cat /proc/pressure/io 2>/dev/null
```

Thresholds: warn at 80 %, act at 85 %, critical at 90 %. No new writes on a disk
with I/O errors.

## 2. Immediate relief (in order, nothing destructive first)

```sh
# 1) safe: old docker build cache + stopped containers
docker builder prune -f
docker container prune -f

# 2) safe: old rotated journal (keep 200 MB)
journalctl --vacuum-size=200M

# 3) old bot logs (container logs are rotated 10 MB x 3 already)
find /opt/bots/chevalet/logs -name '*.log' -mtime +14 -delete 2>/dev/null || true

# 4) old versioned images — keep the live one + the rollback one
docker image ls chevalet-bot
docker rmi chevalet-bot:<old-tag>        # never the live/rollback tags

# 5) backups: retention is automatic; if disk is critical, move OLD artifacts
#    off-host first (offsite-push.sh), verify, only then delete locally.
```

**Never** delete the PostgreSQL data volume or run `docker system prune -a --volumes`.

## 3. Co-tenant data (needs Atur's decision)

The audit found ~7.4 GB of unrelated project data on the same disk
(`/root/verdex-backups` ~6.1 GB, `/root/verdex-staging` ~1.3 GB). Archive it
off-host (encrypted) and only then delete, or keep it with an explicit budget. This
is a blast-radius decision, not a bot decision.

## 4. If I/O errors / a stall is in progress (not just "full")

```sh
# stop new writes to the DB safely: stop the BOT, never the DB
docker stop telegram-bot
# take the newest verified artifact off-host NOW (read-only on the DB)
/opt/bots/chevalet/ops/offsite-push.sh
# then decide with the provider: snapshot/console/rescue
```

- Do **not** reboot blindly while PostgreSQL is writing (fsck/recovery risk);
  follow the provider's console/rescue guidance.
- Prepare the rebuild path ([`DR-HOST-REBUILD.md`](DR-HOST-REBUILD.md)) if the disk
  is dying.
- Open a provider ticket with the dmesg/journal evidence.

## 5. Recovery verification

- `df -h /` < 80 % (target ≥25 % free), `df -i` < 80 %.
- PostgreSQL: `pg_isready` + `select count(*) from users`; no recovery loop in logs.
- Bot: `docker start telegram-bot`; `healthy`, `/readyz` 200, `bot polling`, restart
  count 0; one smoke test.
- Backups: `backup-v2.sh` produces a fresh verified artifact and
  `restore-drill.sh` PASS; offsite push verified again.

## 6. Prevent

- Watchdog already alerts at disk ≥85 % (from outside).
- Keep retention bounded (see [`BACKUP-VERIFY.md`](BACKUP-VERIFY.md)).
- Patch/kernel updates and reboot-required are a separate approval window; an old
  kernel is a storage-bug risk.
- Ask the provider about snapshots; but off-host copies remain the real answer
  (virtio has no SMART).
