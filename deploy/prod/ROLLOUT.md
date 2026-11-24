# Production rollout — v3.4.1 hardening + backup/DR (Germany host)

Phased, reversible, and **no phase runs without Atur's explicit approval**. Every
phase has a pre-check, an acceptance gate and a rollback. The production bot
`@Chevalet_bot` is live; the database must never be put in a position where it is
the only copy.

**Hard rules**

- **Never** `docker compose down` in production — it stops `telegram-bot-db`.
  Stop the bot alone with `docker stop telegram-bot`.
- **Never** restore in place over the live database. `restore-v2.sh prod` restores
  into a NEW database and swaps names; the old one is kept for rollback.
- **Never** run `docker exec -t` for a dump pipeline (`-t` corrupts the stream).
- **Only `chevalet-backup-v2` artifacts are restorable.** `restore-v2.sh` refuses
  dumps containing cluster-level DDL (`DROP/CREATE DATABASE`, `\connect`); the
  legacy `backup_mydatabase_*.sql.gz` files made by the old scripts are such dumps.
  `prod` additionally requires the v2 `manifest.json` sidecar. A legacy file may
  only be converted off-host first — see
  [`../../docs/operations/RESTORE.md`](../../docs/operations/RESTORE.md)
  "Legacy artifacts (emergency only)". There is no v2 artifact until step 1.3
  below has run once.
- **Always** take a verified pre-change backup (`backup-v2.sh --gate`).
- Low-traffic window 04:00–05:00 Tehran; announce it; never during an incident.
- Nothing in this document was executed on production while writing it.

Paths below are the live production values: root `/opt/bots/chevalet`, project
`chevalet`, containers `telegram-bot` (bot) and `telegram-bot-db` (Postgres 16),
data volume expected `chevalet_postgres_data`, public host Germany
`181.41.194.149`, SSH port 9011.

---

## Phase 0 — pre-flight capture (READ-ONLY; zero downtime)

Run on the production host. Everything here is read-only except the two local
copies in step 3, which are additive.

- [ ] **0.1 Capture the live stack before changing anything** (the audit could not
      see the live compose; do not merge a guessed file over it):

      ```sh
      mkdir -p /opt/bots/chevalet/ops-captured/{compose,env-metadata,inspect}
      chmod 700 /opt/bots/chevalet/ops-captured
      cp -a /opt/bots/chevalet/docker-compose.yml*        /opt/bots/chevalet/ops-captured/compose/ 2>/dev/null || true
      ls -la /opt/bots/chevalet/                          > /opt/bots/chevalet/ops-captured/ls.txt
      docker inspect telegram-bot    > /opt/bots/chevalet/ops-captured/inspect/telegram-bot.json
      docker inspect telegram-bot-db > /opt/bots/chevalet/ops-captured/inspect/telegram-bot-db.json
      docker compose -p chevalet config > /opt/bots/chevalet/ops-captured/compose/resolved.yml 2>/dev/null || true
      docker images --digests | head -20                 > /opt/bots/chevalet/ops-captured/images.txt
      df -h /; df -i /; docker system df                 > /opt/bots/chevalet/ops-captured/disk.txt
      crontab -l                                          > /opt/bots/chevalet/ops-captured/cron.txt 2>&1 || true
      ls -la /opt/bots/chevalet/backups/ | tail -5        > /opt/bots/chevalet/ops-captured/backups.txt
      ```
      Do **not** record `.env` contents (secrets); record only its keys:
      `sudo grep -o '^[A-Za-z_][A-Za-z0-9_]*=' /opt/bots/chevalet/.env | tr -d '=' | sort`.

- [ ] **0.2 Verify the data volume name — do not let compose create an empty DB**:

      ```sh
      docker volume ls | grep -i postgres          # expect: chevalet_postgres_data
      docker inspect telegram-bot-db --format '{{range .Mounts}}{{.Type}} {{.Name}} -> {{.Destination}}{{"\n"}}{{end}}'
      ```
      `deploy/prod/docker-compose.live.yml` declares a project-scoped volume
      `postgres_data`, which resolves to `chevalet_postgres_data` under project
      `chevalet` — i.e. the existing volume. **If the names differ, pin it before
      phase 2**:

      ```yaml
      volumes:
        postgres_data:
          name: <the-existing-volume-name-from-docker-volume-ls>
      ```
      Also note `docker compose up` must be run with `--no-deps telegram-bot`
      (Phase 2) so the DB service is never recreated by a config diff.

- [ ] **0.3 State-dir migration for `dynamic_settings.json`** (required BEFORE the
      bot is recreated with the new compose, otherwise admin settings are lost or
      startup validation fails):

      ```sh
      # today: /opt/bots/chevalet/dynamic_settings.json (600, content "{}"), no state/ dir
      install -d -m 700 /opt/bots/chevalet/state
      cp -a /opt/bots/chevalet/dynamic_settings.json /opt/bots/chevalet/state/dynamic_settings.json
      chmod 600 /opt/bots/chevalet/state/dynamic_settings.json
      # keep the old location as an explicit backup (do not delete it)
      cp -a /opt/bots/chevalet/dynamic_settings.json /opt/bots/chevalet/dynamic_settings.json.pre-v3.4.1.bak
      ls -l /opt/bots/chevalet/state/dynamic_settings.json /opt/bots/chevalet/dynamic_settings.json*
      ```
      The live compose sets `DYNSET_PATH=/app/state/dynamic_settings.json` and
      mounts `/opt/bots/chevalet/state:/app/state`. Verify **after** the recreate
      in Phase 2: `docker exec telegram-bot cat /app/state/dynamic_settings.json`
      (must show the preserved content, not a fresh empty file).

- [ ] **0.4 `.env` key audit** (the old compose whitelist silently dropped 11 keys,
      so the prod `.env` may be missing them too):

      ```sh
      grep -o '^[A-Za-z_][A-Za-z0-9_]*=' /opt/bots/chevalet/.env | tr -d '=' | sort > /tmp/env-keys.txt
      diff <(grep -o '^[A-Za-z_][A-Za-z0-9_]*=' .env.example | tr -d '=' | sort) /tmp/env-keys.txt
      ```
      Expected missing keys at minimum: `BACKUP_CHAT_ID`, `BACKUP_TIME`, `AI_ENABLED`,
      `DYNSET_PATH`, `BATCH_*`, `ALBUM_WINDOW_MS`, `CONTINUE_WINDOW_MS`, `TOKEN_KEYS`,
      `DB_PORT` (plus the new v3.4.1 keys `DB_SSLMODE`, `DB_CONNECT_TIMEOUT_SECONDS`,
      `DB_MAX_CONNS`, `HEALTH_WATCHDOG_SECONDS`, `HEARTBEAT_URL` in Phase 4).
      Add values deliberately (see `.env.example` for safe defaults); the compose
      `env_file: .env` change in Phase 2 makes future keys flow automatically.

- [ ] **0.5 Gate:** captured artifacts exist in `ops-captured/`; volume name
      confirmed; state copy exists; key-diff list reviewed. **Rollback: none needed
      (no service changed).**

---

## Phase 1 — install verification + off-host DR (ZERO downtime)

- [ ] **1.1 Install the ops scripts** from a reviewed release checkout
      (repo tag; do not copy from a dirty tree):

      ```sh
      install -d -m 700 /opt/bots/chevalet/ops
      install -m 755 /path/to/repo/deploy/prod/ops/*.sh /opt/bots/chevalet/ops/
      # systemd units
      install -m 644 /path/to/repo/deploy/prod/systemd/chevalet-*.service \
                     /path/to/repo/deploy/prod/systemd/chevalet-*.timer /etc/systemd/system/
      chmod 644 /etc/systemd/system/chevalet-alert@.service
      systemctl daemon-reload
      ```

- [ ] **1.2 `/etc/chevalet/ops.env`** (root:root 600) with the dedicated alert bot
      (`ALERT_TOKEN`, `ALERT_CHAT_ID`), ONE off-host destination (`RCLONE_REMOTE`
      and/or `OFFSITE_DIR` — Cloudflare R2 / Backblaze B2 via rclone, or an
      encrypted external mirror; **no Arvan dependency**), optional `AGE_RECIPIENT`
      and `HC_PING_URL`. Template: [`README.md`](README.md#environment-for-the-ops-scripts).

- [ ] **1.3 First certified artifact + drill (before enabling timers):**

      This is the first `chevalet-backup-v2` artifact; until it exists, the only
      dumps on the host are legacy `--create` artifacts that `restore-v2.sh` will
      (correctly) refuse. Do not point `restore-v2.sh prod` at a legacy file: convert
      it off-host first per `docs/operations/RESTORE.md`.

      ```sh
      /opt/bots/chevalet/ops/backup-v2.sh
      ls -la /opt/bots/chevalet/backups-v2/published/            # .sql.gz + .sha256 + .manifest.json, 600
      /opt/bots/chevalet/ops/restore-drill.sh                    # must print RESULT: PASS
      ```

- [ ] **1.4 First off-host copy + verification:**

      ```sh
      /opt/bots/chevalet/ops/offsite-push.sh                     # must log "verified" / "checksum(s) verified"
      # object storage:   rclone ls "$RCLONE_REMOTE" | tail -5
      # directory mirror: ls -la "$OFFSITE_DIR" | tail -5
      ```
      If neither destination is possible yet, the script exits 0 but writes
      `backups-v2/state/OFFSITE-NOT-CONFIGURED` and logs a loud ERROR: off-host RPO
      is then **unmet**, and that must be recorded as an open risk (do not report
      the P0 layer done).

- [ ] **1.5 Enable the timers:**

      ```sh
      systemctl enable --now chevalet-backup.timer chevalet-offsite.timer chevalet-drill.timer
      systemctl list-timers --all | grep chevalet
      journalctl -u chevalet-backup -n 30 --no-pager
      ```

- [ ] **1.6 Migrate away from the legacy cron safely:** keep the old
      `backup-tiered.sh` cron for 24 h alongside the new 15-min timer, compare both
      directories, then remove the old cron line. No container restart involved.

- [ ] **1.7 Gate:** artifact every ≤15 min, manifest+sha256, drill PASS, off-host
      copy verified from a **different** medium/provider, drill from the off-host
      copy PASS on the workstation. **Rollback:** disable the new timers and keep
      the old cron (nothing production-facing changed).

---

## Phase 2 — reconcile the compose stack (bot-only restart; maintenance window)

Purpose: make the running stack match `deploy/prod/docker-compose.live.yml` so
`env_file: .env` fixes the dropped-key class (H-9), the HTTP healthcheck replaces
the meaningless TCP probe, and state/backups mounts are persistent.

- [ ] **2.1 Pre-flight:** Phase 0 complete; `backup-v2.sh --gate` exits 0
      (a fresh verified artifact < 90 min old); note the current image
      `docker inspect telegram-bot --format '{{.Image}}'` and container start time;
      announce the window.

- [ ] **2.2 Confirm the `.env` now contains the keys from 0.4** and that
      `DYNSET_PATH` is NOT set to a container-only path in `.env` (the compose
      override sets it; if the host `.env` also sets it, the `.env` value is
      overridden by the compose `environment:` entry — verify after start).

- [ ] **2.3 Install the compose file (captured copy already in ops-captured/):**

      ```sh
      install -m 600 /path/to/repo/deploy/prod/docker-compose.live.yml /opt/bots/chevalet/docker-compose.yml
      cd /opt/bots/chevalet
      docker compose -p chevalet config >/dev/null          # must parse; never shows secrets in the log
      ```

- [ ] **2.4 Recreate ONLY the bot** (this is the load-bearing `--no-deps`: even if
      the DB service definition differs, the database container/volume is left
      untouched):

      ```sh
      cd /opt/bots/chevalet
      IMAGE_TAG=latest docker compose -p chevalet up -d --no-deps telegram-bot
      docker ps --format '{{.Names}} {{.Status}}' | grep telegram
      ```
      The DB container must show its ORIGINAL uptime (it was not restarted). If it
      was recreated, STOP and restore from the captured inspection/volume before
      continuing.

- [ ] **2.5 Verify the bot after recreate:**
      - `docker inspect telegram-bot --format '{{.State.Health.Status}}'` → `healthy`
        (the new healthcheck probes HTTP `/healthz`; a pre-v3.4.1 image will fail
        this — so this phase pairs with Phase 4, or stays on the old `nc` probe
        until the image is upgraded; do not leave a red healthcheck in place),
      - `docker logs telegram-bot --tail 20` shows `bot polling`,
      - `docker exec telegram-bot cat /app/state/dynamic_settings.json` preserves
        the admin state; `/admin` → AI settings still show the configured values,
      - `docker exec telegram-bot-db psql -U root -d mydatabase -tAc 'select count(*) from users'`
        is within a few rows of the pre-restart count.

- [ ] **2.6 Gate:** bot healthy + polling, DB never restarted, admin settings
      intact. **Rollback:** `IMAGE_TAG=latest docker compose -p chevalet up -d --no-deps telegram-bot`
      with the captured compose file restored from `ops-captured/compose/`; the
      state file copy in 0.3 remains available.

---

## Phase 3 — re-establish the tag-driven deploy path (currently broken since migration)

The forced-command deploy script in `deploy/go/deploy-tag.sh` targets the OLD host
(`/opt/chevalet-go-staging`, project `go`, container `chevalet-go-bot-prod`). CI's
deploy job is therefore **gated off** (`vars.DEPLOY_ENABLED != 'true'`). Until it
is re-established, ship v3.4.1 with the controlled manual path in Phase 4.

- [ ] 3.1 Adapt the deploy script to the new host (`REPO=/opt/bots/chevalet`,
      `COMPOSE_FILE=docker-compose.yml`, `PROJECT=chevalet`, `CONTAINER=telegram-bot`,
      `IMAGE=chevalet-bot`, `LOGFILE=/var/log/chevalet-deploy.log`) — adapted copy
      approved separately.
- [ ] 3.2 Install `/usr/local/bin/deploy-chevalet.sh` (755), add the deploy key to
      `/root/.ssh/authorized_keys` with
      `command="/usr/local/bin/deploy-chevalet.sh",no-agent-forwarding,no-X11-forwarding,no-port-forwarding,no-pty`.
- [ ] 3.3 Update GitHub secrets `DEPLOY_HOST/PORT/USER/SSH_KEY`; keep `production`
      environment protection rules.
- [ ] 3.4 Rehearse with a throwaway tag: deploy → auto-rollback → deploy previous
      tag; check `/var/log/chevalet-deploy.log`.
- [ ] 3.5 Only then set repository variable `DEPLOY_ENABLED=true`.
- [ ] **Gate:** throwaway tag deployed and rolled back cleanly. **Rollback:** set
      `DEPLOY_ENABLED=false` again (the manual path stays available).

---

## Phase 4 — deploy v3.4.1 (controlled manual path if Phase 3 is not done)

- [ ] **4.1 Pre-deploy gate — hard requirement:**

      ```sh
      /opt/bots/chevalet/ops/backup-v2.sh --gate     # exit != 0 => ABORT the deploy
      /opt/bots/chevalet/ops/offsite-push.sh         # refresh the off-host copy
      /opt/bots/chevalet/ops/restore-drill.sh        # PASS on the newest artifact
      ```

- [ ] **4.2 Get the v3.4.1 image onto the host** (git checkout/tag deploy path when
      Phase 3 is done; otherwise from a CI-verified local build):

      ```sh
      # on the workstation, from the reviewed commit of the release tag:
      go build ./... && go test -race -count=1 ./... && gofmt -l .   # expect no output
      docker build -t chevalet-bot:v3.4.1 .
      docker save chevalet-bot:v3.4.1 | gzip -9 | ssh -p 9011 root@181.41.194.149 'gunzip | docker load'
      ssh -p 9011 root@181.41.194.149 'docker image inspect chevalet-bot:v3.4.1 --format "{{.Id}} {{.Created}}"'
      ```
      Record the image ID in the release log.

- [ ] **4.3 Switch the bot to the new image** (bot container only; DB untouched):

      ```sh
      cd /opt/bots/chevalet
      IMAGE_TAG=v3.4.1 docker compose -p chevalet up -d --no-deps telegram-bot
      ```

- [ ] **4.4 Verify (acceptance gate):**
      - `docker inspect telegram-bot --format '{{.State.Health.Status}}'` → `healthy`
        (HTTP `/healthz` now real; also `wget -qO- http://127.0.0.1:12564/readyz`
        from inside the container must be 200),
      - `docker logs telegram-bot --since "$(docker inspect -f '{{.State.StartedAt}}' telegram-bot)" --tail 50`
        shows `bot polling`, no `panic`, no FATAL, and the resolved (non-secret)
        config lines including `DB_SSLMODE`, `DB_MAX_CONNS`, `HEALTH_WATCHDOG`,
      - `docker inspect telegram-bot --format '{{.RestartCount}}'` stays 0 over
        10 minutes,
      - one real user smoke test (send/reply/seen/block) from a test account,
      - `docker exec telegram-bot-db psql -U root -d mydatabase -tAc 'select count(*) from users'`
        within ±2 % of the pre-deploy count,
      - admin settings (`/admin` AI url/session, donation) intact.

- [ ] **4.5 Rollback (any gate fails):**

      ```sh
      cd /opt/bots/chevalet
      IMAGE_TAG=latest docker compose -p chevalet up -d --no-deps telegram-bot
      # or, if the tag was the previous one: IMAGE_TAG=<previous-tag> …
      docker inspect telegram-bot --format '{{.State.Health.Status}}'
      ```
      The previous image is retained on disk (never prune it before the release is
      accepted). Database rollback is NOT part of a code rollback; only a bad
      data-changing operation needs the `restore-v2.sh prod` procedure (see
      `docs/operations/RESTORE.md`).

- [ ] **4.6 Post-deploy:** append a release record (tag, image ID, artifact sha,
      deploy time, verifier) to the rollout log; update the audit
      `PLAN.md` IMPLEMENTATION STATUS section.

---

## Phase 5 — external monitoring (no production change)

- [ ] 5.1 Install `watchdog.sh` + units on Atur's workstation
      ([`systemd/README.md`](systemd/README.md)); first run with `--no-alert`.
- [ ] 5.2 Register the healthchecks.io dead-man switch (`HC_PING_URL` on prod);
      alert to the same ops chat if pings stop.
- [ ] 5.3 Exercise the alerts in an approved window: stop `telegram-bot` for 2 min
      → watchdog alert within 5 min; start it → recovery alert. Then simulate a
      stale backup (pause the timer >90 min, restore it) → alert.
- [ ] **Gate:** each alert received on Atur's phone via the dedicated alert bot.

---

## Rollback matrix

| Change | Rollback |
| --- | --- |
| v3.4.1 image | `IMAGE_TAG=<previous> docker compose -p chevalet up -d --no-deps telegram-bot` |
| compose/healthcheck | restore captured compose from `ops-captured/compose/`, same up command |
| dynamic_settings state | old file kept at `dynamic_settings.json.pre-v3.4.1.bak`; copy back into `state/` |
| backup/offsite timers | `systemctl disable --now chevalet-offsite.timer chevalet-backup.timer`; old cron kept 24 h |
| database content | `restore-v2.sh prod <artifact> --approve` (new DB + swap; old DB kept) — see `docs/operations/RESTORE.md` |
| deploy pipeline | set `DEPLOY_ENABLED=false`; manual image path remains |

## Acceptance — "the P0 layer cannot silently lose everything again"

- [ ] 3-2-1 copies exist and the newest artifact is ≤30 min old off-host.
- [ ] Monthly/daily automated drill PASS recorded (`backups-v2/drills/last-PASS`).
- [ ] External detection ≤5 min for bot down / hung / DB down / backup stale /
      disk >85 %, proven by test.
- [ ] v3.4.1 lives through the restored pipeline (or the documented manual path)
      with a pre-deploy backup gate and a one-command image rollback.
- [ ] All runbooks in `docs/operations/` checked against the actual host.
