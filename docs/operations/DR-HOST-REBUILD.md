# RB-03 — Disaster recovery: rebuild on a fresh host (total host loss)

Use when `PROD_HOST` is gone/unrecoverable (the 2026-11-23 class). Target RTO ≤45 min
once the off-host copies exist. Preconditions that this runbook assumes:

- newest off-host DB artifact (`RCLONE_REMOTE` or `OFFSITE_DIR`) with `.sha256` +
  `.manifest.json`,
- config bundle off-host: `.env` (from the vault), compose file, `Texts/`,
  `state/dynamic_settings.json`, current image digest (or the release tag),
- `BOT_TOKEN` + DB creds from the vault,
- this repo at the release tag.

## 1. Provision the new host

Any provider with Docker; 2 vCPU / 2 GB+ / 25 GB disk is sufficient for the bot
(current usage ~8.5 MB bot + 39 MB DB, disk 72 % full on the old 25 GB host incl.
co-tenant data). Install Docker Engine + compose plugin. Create the SSH access
Atur uses (key-only), open the SSH port.

## 2. Lay out the stack (≈10 min)

```sh
export CHEVALET_ROOT=/opt/bots/chevalet
install -d -m 700 "$CHEVALET_ROOT"/{state,ops,backups-v2}
cd "$CHEVALET_ROOT"
# compose file of record (from the repo at the release tag)
install -m 600 /path/to/repo/deploy/prod/docker-compose.live.yml ./docker-compose.yml
# bring .env from the vault (never from a chat/log): must be 600 root
install -m 600 /path/to/vault/.env ./.env
# Texts/ and the state file are in the config bundle
tar -xzf /path/to/bundle/texts.tar.gz -C .
install -m 600 /path/to/bundle/dynamic_settings.json ./state/dynamic_settings.json
install -m 755 /path/to/repo/deploy/prod/ops/*.sh ./ops/
```

Start PostgreSQL only, **before** the bot (so the restore target exists and the bot
does not crash-loop against an empty schema):

```sh
cd "$CHEVALET_ROOT"
docker compose -p chevalet up -d postgres
docker inspect telegram-bot-db --format '{{.State.Health.Status}}'
```

Note: the fresh host has a **new** empty volume — that is expected; the data comes
from the off-host artifact in step 3.

## 3. Restore the database (≈10–20 min)

Fetch the newest artifact + companions from the off-host copy, verify, then restore
into a NEW database and swap exactly as in production. On a **fresh** host the
simplest path is the documented drill semantics (there is no data to preserve), but
still verify before starting the bot:

```sh
rclone copy "$RCLONE_REMOTE/<artifact>.sql.gz" /root/restore/
rclone copy "$RCLONE_REMOTE/<artifact>.sql.gz.sha256" /root/restore/
rclone copy "$RCLONE_REMOTE/<artifact>.sql.gz.manifest.json" /root/restore/
/opt/bots/chevalet/ops/restore-v2.sh verify /root/restore/<artifact>.sql.gz

# fresh-host restore: restore into the container's database
gunzip -c /root/restore/<artifact>.sql.gz | \
  docker exec -i telegram-bot-db psql -U root -d mydatabase -v ON_ERROR_STOP=1 -q
docker exec telegram-bot-db psql -U root -d mydatabase -tAc \
  "select (select count(*) from users), (select count(*) from cids), (select count(*) from information_schema.tables where table_schema='public');"
```

Counts must match the manifest (`users`/`cids`) within tolerance and show 8 tables.

## 4. Start the bot + verify (≈5 min)

```sh
cd /opt/bots/chevalet
docker compose -p chevalet up -d telegram-bot
docker inspect telegram-bot --format '{{.State.Health.Status}}'        # healthy
docker exec telegram-bot wget -qO- http://127.0.0.1:12564/readyz; echo # 200
docker logs telegram-bot --since "$(docker inspect -f '{{.State.StartedAt}}' telegram-bot)" | grep -c 'bot polling'
```

- `bot polling` present, no panic/FATAL, restart count 0,
- smoke test from a test account,
- old inline buttons on pre-incident messages still work (cipher/`TOKEN_KEYS`).

## 5. Re-enable the safety layers (≈5 min)

```sh
install -d -m 700 /etc/chevalet && install -m 600 /path/to/vault/ops.env /etc/chevalet/ops.env
install -m 644 /path/to/repo/deploy/prod/systemd/chevalet-*.service \
               /path/to/repo/deploy/prod/systemd/chevalet-*.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now chevalet-backup.timer chevalet-offsite.timer chevalet-drill.timer
/opt/bots/chevalet/ops/backup-v2.sh        # push the first artifact from the new host
/opt/bots/chevalet/ops/offsite-push.sh
/opt/bots/chevalet/ops/restore-drill.sh    # PASS
```

Then re-check the external watchdog (`PROD_HOST` change on the monitoring host) and
the healthchecks.io dead-man switch.

## 6. Close out

- Update DNS/notifications if the address changed; re-point the watchdog.
- Record actual RTO in the brain journal; note any gap between the target and the
  result, and fix the runbook if a step was missing.
- Keep the old release record/image so rollback remains possible.

## Pitfalls (learned from 2026-11-23)

- The only surviving copy must not be on the dead host — that is why the off-host
  push and the workstation mirror exist.
- Do not restore the DB before the bot is stopped/recreated cleanly; two pollers on
  one token fail with HTTP 409.
- Wrong `DYNSET_PATH`/state mount silently resets admin settings — restore
  `state/dynamic_settings.json` from the bundle.
- A dump produced with `docker exec -t` can contain CRLF/merged stderr; the v2
  backup scripts avoid it. Prefer v2 artifacts.
