# RB-06 — Backup verification (daily checks + monthly drill)

A backup only counts once it has restored. This runbook is the routine proof.

## What "verified" means

`backup-v2.sh` publishes an artifact only after all of these pass:

| Check | Requirement |
| --- | --- |
| size | ≥ 1,000,000 bytes (production dump ~2.1–2.8 MB) |
| gzip | `gzip -t` passes |
| footer | contains `-- PostgreSQL database dump complete` |
| structure | ≥ 8 `CREATE TABLE` blocks |
| checksum | `.sha256` sidecar written; sha256 in `.manifest.json` agrees |
| atomic | temp file verified first, then renamed into `published/`; 700/600 perms |

`offsite-push.sh` then copies to the destination and verifies the **remote/local**
sha256 before recording success. An artifact that is only on the production disk is
**not** a backup (that was the 2026-11-23 loss).

## Daily checks (automated; a human reads the result)

```sh
# production host
ls -la /opt/bots/chevalet/backups-v2/published/ | tail -6
cat /opt/bots/chevalet/backups-v2/published/<newest>.manifest.json
cat /opt/bots/chevalet/backups-v2/drills/last-PASS
tail -20 /opt/bots/chevalet/backups-v2/log/backup.log
tail -20 /opt/bots/chevalet/backups-v2/log/offsite.log
state="$(tail -1 /opt/bots/chevalet/backups-v2/state/offsite.status 2>/dev/null)"; echo "$state"
test ! -e /opt/bots/chevalet/backups-v2/state/OFFSITE-NOT-CONFIGURED || echo "OFF-HOST RPO UNMET"
```

Expected: newest artifact ≤30 min old (15-min timer), manifest present, drill
`last-PASS` from today/this week, offsite log says verified, no
`OFFSITE-NOT-CONFIGURED` marker.

If the watchdog alerts with "backup stale": check `systemctl status
chevalet-backup.timer`, `journalctl -u chevalet-backup -n 100`, the DB container
state, and disk. A backup that keeps failing is treated as an incident
([`INCIDENT-TRIAGE.md`](INCIDENT-TRIAGE.md)) because the RPO is now growing.

## Manual verification (any time, no production change)

```sh
# on the host — the drill only reads published/ and creates a throwaway container
/opt/bots/chevalet/ops/backup-v2.sh --gate
/opt/bots/chevalet/ops/restore-drill.sh                       # RESULT: PASS
/opt/bots/chevalet/ops/restore-v2.sh verify <artifact>         # VERIFY PASS
```

On the workstation, prove the **off-host** copy restores (the copy that matters):

```sh
rclone copy "$RCLONE_REMOTE/<artifact>.sql.gz" /tmp/check/     # or cp from OFFSITE_DIR
rclone copy "$RCLONE_REMOTE/<artifact>.sql.gz.sha256" /tmp/check/
rclone copy "$RCLONE_REMOTE/<artifact>.sql.gz.manifest.json" /tmp/check/
cd <repo> && BACKUP_ROOT=/tmp/check-root ./deploy/prod/ops/restore-drill.sh /tmp/check/<artifact>.sql.gz
# artifact must be under <BACKUP_ROOT>/published for the default lookup —
# or pass the explicit path as above
```

Expected `RESULT: PASS` with `tables=8 users>0 cids>=users bad_uids=0 dup_cids=0`.

## Monthly / quarterly cadence

| Frequency | What |
| --- | --- |
| Daily (automated) | 15-min backups, off-site push, daily drill at 04:30, watchdog state |
| Weekly | read `last-PASS` + newest manifest; spot-check one restore of the off-host copy |
| Monthly | full drill from the off-host artifact on the workstation; retention/disk review; patch window |
| Quarterly | full DR rehearsal from off-host copies on a **fresh** host ([`DR-HOST-REBUILD.md`](DR-HOST-REBUILD.md)); record measured RTO |

## Retention (automatic, deterministic)

`backup-v2.sh` keeps hourly artifacts for 48 h, the newest per UTC day for 14 d, the
newest per ISO week for 8 w, and always the newest artifact. Budget: ~2.8 MB per
artifact every 15 min → ≈10 GB if nothing were pruned; with this policy the local
footprint stays ≈0.5–1 GB. Check occasionally: `du -sh /opt/bots/chevalet/backups-v2`.

## Don'ts

- Do not rely on the bot's nightly Telegram send as the only off-host copy — it is
  tertiary, single-destination, and depends on the bot's own token path.
- Do not edit/rotate the `.sha256` or manifest by hand.
- Do not delete `published/` while a drill or push may be running (the scripts use
  `flock`, but a manual `rm` bypasses it).
- Do not restore an artifact whose sha256 does not match its sidecar, even if it
  "looks like" a valid gzip.
