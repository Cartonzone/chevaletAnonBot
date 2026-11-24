# RB-02 — Restore the database (tested procedure)

**Destructive to the live database.** Requires Atur's explicit approval. The
2026-11-23 incident is why this exists: the old procedure restored in place with no
safety dump. Use `deploy/prod/ops/restore-v2.sh` — it restores into a NEW database,
verifies invariants, then swaps names and keeps the previous database for rollback.

Never `docker compose down`. Never restore over the live DB in place.

> **Only `chevalet-backup-v2` artifacts are accepted.** `restore-v2.sh` refuses
> artifacts containing cluster-level DDL (`DROP DATABASE`, `CREATE DATABASE`,
> `ALTER DATABASE … OWNER`, `\connect`) and, in `prod` mode, requires the
> `<artifact>.manifest.json` sidecar with `"format": "chevalet-backup-v2"`. Legacy
> dumps made by the old scripts (`pg_dump --clean --if-exists --create`) contain
> `DROP DATABASE mydatabase; CREATE DATABASE mydatabase; \connect mydatabase`:
> fed to `psql -d <new-db>`, those lines act on the **whole cluster** and can drop
> the live database before any invariant check runs. They are emergency-only —
> convert them first (see [Legacy artifacts (emergency only)](#legacy-artifacts-emergency-only)).

## 0. Choose the artifact (off-host copy first)

Prefer the newest artifact from the **off-host** copy (object storage / mirror), not
the same disk that may itself be failing:

```sh
# object storage
rclone ls "$RCLONE_REMOTE" | sort | tail -5
rclone copy "$RCLONE_REMOTE/<artifact>.sql.gz"        /root/restore/
rclone copy "$RCLONE_REMOTE/<artifact>.sql.gz.sha256" /root/restore/
rclone copy "$RCLONE_REMOTE/<artifact>.sql.gz.manifest.json" /root/restore/
# directory mirror
ls -la "$OFFSITE_DIR" | tail -5
# then verify BEFORE touching production
deploy/prod/ops/restore-v2.sh verify /root/restore/<artifact>.sql.gz
```

Check the manifest: `created_utc`, `tables`, `live_users`. Decide what data loss
window is acceptable (a ≤15-min-old artifact loses at most 15 min of writes).
`verify` must print `RESULT: VERIFY PASS`; a legacy artifact exits non-zero with
`contains cluster-level DDL` — that is the safety gate, not a corrupt file.

## 1. Safety dump + preconditions (on the production host)

```sh
/opt/bots/chevalet/ops/backup-v2.sh            # fresh pre-restore artifact
/opt/bots/chevalet/ops/backup-v2.sh --gate     # must exit 0
/opt/bots/chevalet/ops/restore-drill.sh        # must print RESULT: PASS
```

If the DB is unhealthy enough that the backup fails, stop and escalate: a restore
without a safety dump turns a recoverable incident into data loss.

## 2. Drill the exact artifact on the host (no production touch)

```sh
/opt/bots/chevalet/ops/restore-v2.sh drill /root/restore/<artifact>.sql.gz
```

## 3. Production restore (approved maintenance window)

```sh
/opt/bots/chevalet/ops/restore-v2.sh prod /root/restore/<artifact>.sql.gz --approve
```

Interactive confirmation requires typing the database name. Scripted/automated
runs additionally need `--yes` (still requires `--approve`). What it does:

1. verifies the artifact (sha256/gzip/footer/table-count/manifest, refuses
   cluster-level DDL — legacy `--create` dumps — and requires the v2 manifest),
2. takes a fresh pre-restore dump via `backup-v2.sh` and aborts if it fails,
3. stops ONLY `telegram-bot` (never the DB, never compose down),
4. creates `mydatabase_restore_<ts>`, restores with `psql -v ON_ERROR_STOP=1`,
5. verifies tables/users/cids/bad-uids/dup-cids/manifest tolerance,
6. renames live → `mydatabase_pre_restore_<ts>` and new → `mydatabase`,
7. starts the bot and waits for `healthy` + `bot polling`,
8. prints exact rollback commands and writes a record to
   `backups-v2/restores/restore-<ts>.json`.

## 4. Verify after restore

```sh
docker inspect telegram-bot --format '{{.State.Health.Status}}'
docker exec telegram-bot wget -qO- http://127.0.0.1:12564/readyz; echo
docker exec telegram-bot-db psql -U root -d mydatabase -tAc 'select count(*) from users'
docker logs telegram-bot --since "$(docker inspect -f '{{.State.StartedAt}}' telegram-bot)" --tail 50
```

Then a real smoke test from a test account (send/reply/seen/block/report) and check
that old inline buttons on pre-incident messages still work (cipher contract).

## 5. Rollback (if the restored data must be reverted)

The script prints the exact commands; generically:

```sh
docker stop telegram-bot
docker exec telegram-bot-db psql -U root -d postgres -v ON_ERROR_STOP=1 \
  -c 'ALTER DATABASE "mydatabase" RENAME TO "mydatabase_restore_FAILED_<ts>";' \
  -c 'ALTER DATABASE "mydatabase_pre_restore_<ts>" RENAME TO "mydatabase";'
docker start telegram-bot
```

## 6. Cleanup (only after acceptance)

Drop stale `*_pre_restore_*` / `*_restore_FAILED_*` databases after the acceptance
period (keep at least the newest one). Verify the failed artifact/database name
before dropping anything:

```sh
docker exec telegram-bot-db psql -U root -d postgres -c '\l'
# DROP DATABASE "<name>";   <- only after checking Backups + Atur approval
```

## Legacy artifacts (emergency only)

Until the first `backup-v2.sh` run (ROLLOUT Phase 1.3) the only artifacts that
exist are legacy `backup_mydatabase_*.sql.gz` dumps from the old scripts. They are
**refused** by `restore-v2.sh verify/drill/prod` on purpose. To ever restore one,
convert it to a `chevalet-backup-v2`-shaped artifact **off the production host**
(throwaway container; the dump's `DROP/CREATE DATABASE` then only affects that
container):

```sh
# 1. Throwaway PostgreSQL — NOT on the production host, no published ports needed
#    beyond loopback, no volume.
docker run -d --name legacy-convert -e POSTGRES_PASSWORD=convert   -p 127.0.0.1::5432 postgres:16-alpine
PORT=$(docker port legacy-convert 5432/tcp | head -1 | cut -d: -f2)

# 2. Legacy dump restores ITS OWN database (mydatabase) inside the throwaway
#    container; ON_ERROR_STOP so a broken dump fails loudly.
gunzip -c /path/to/backup_mydatabase_<ts>.sql.gz |
  PGPASSWORD=convert psql -h 127.0.0.1 -p "$PORT" -U postgres -d postgres     -v ON_ERROR_STOP=1 -q
PGPASSWORD=convert psql -h 127.0.0.1 -p "$PORT" -U postgres -d mydatabase -tAc   'select count(*) from users'    # sanity: must match the dump's known user count

# 3. Re-dump WITHOUT --create/--clean (this is exactly what backup-v2.sh does) and
#    build the v2 certification files.
STAMP=$(date -u +%Y%m%dT%H%M%SZ); ART="backup_mydatabase_${STAMP}.sql.gz"
PGPASSWORD=convert pg_dump -h 127.0.0.1 -p "$PORT" -U postgres -d mydatabase   --no-owner --no-privileges --encoding=UTF8 | gzip > "$ART"
SIZE=$(stat -c %s "$ART"); SHA=$(sha256sum "$ART" | cut -d' ' -f1); USERS=... # from step 2
printf '%s  %s
' "$SHA" "$ART" > "$ART.sha256"
cat > "$ART.manifest.json" <<JSON
{ "format": "chevalet-backup-v2", "created_utc": "$(date -u +%FT%TZ)",
  "host": "legacy-conversion", "db": "mydatabase", "db_user": "postgres",
  "dump_format": "plain-sql", "compression": "gzip", "size_bytes": ${SIZE},
  "tables": 8, "live_users": ${USERS}, "sha256": "${SHA}",
  "pg_dump_version": "pg_dump (PostgreSQL) 16.x" }
JSON
chmod 600 "$ART" "$ART.sha256" "$ART.manifest.json"
docker rm -f legacy-convert
```

Then `restore-v2.sh verify` (must PASS), `restore-v2.sh drill` (must PASS) and only
then the normal `prod` flow. Never feed the **legacy** file to `prod`, and never
run the conversion on the database container itself.

## Restore drill (daily, automated)

`chevalet-drill.timer` runs `restore-drill.sh` daily at 04:30. Last known-good
proof: `cat /opt/bots/chevalet/backups-v2/drills/last-PASS`. A failing drill always
alerts. See [`BACKUP-VERIFY.md`](BACKUP-VERIFY.md).
