# Release v3.4.1 — production hardening (release notes / runbook)

Status: **release candidate on branch `hardening/v3.4.1`. Not tagged, not
committed to production, nothing deployed.** Production `@Chevalet_bot` is still
v3.4.0 while this branch is prepared and reviewed.

Base: `v3.4.0` (production tree verified byte-identical to that tag).
Scope: the CODE-AUDIT `v3.4.1` set (C-1/C-1b, H-1/H-3/H-6/H-8/H-9, M-6,
backup-name fix) + the P0 data/DR/ops layer (own scripts, CI, docs). The
structural `v3.5` work (per-update deadlines, DB circuit breaker, migrations,
metrics) is **out of scope** here.

---

## 1. Runtime/code changes on this branch

| Area | Change | Effect |
| --- | --- | --- |
| DB error classification (C-1) | `isDBError` now recognises `pgconn.Timeout` (the incident's exact `timeout: context deadline exceeded`) and `context.DeadlineExceeded` from pool acquire | a DB stall goes through the polite DB path instead of an unthrottled incident + tracking-code flood |
| Incident throttle (C-1b) | one aggregated incident per 30 s window, suppressed counts folded into the next message | any future failure class can no longer flood `ERROR_CHAT_ID` |
| Bounded startup (H-1) | health listener opens first; DB connect retries bounded (~60 s) then exit 1; connect timeout 5 s default | boot hang becomes a visible crash-loop instead of an invisible half-start; restart policy heals |
| Runtime asset validation (H-8) | `Texts/`, backups dir and DYNSET parent validated before polling; failure = exit 1 | "healthy but erroring every update" fails closed |
| Timeouts (H-6) | API default stays 10 s; long timeout only for backup upload | no 30 s stalls on ordinary calls |
| Callback safety (M-6) | callback queries are answered immediately in the DB-degraded fast-fail branch; the normal path keeps each handler's own answer so popup alerts still work | no "query is too old" during a DB stall without double-answering/breaking popups; with the 5 s interactive DB budget, answers stay inside Telegram's ~10 s window |
| Health semantics (C-3 support) | HTTP server on `127.0.0.1:$HEALTH_PORT`: `GET /healthz` (alias `/health`) liveness = process + recent successful `getUpdates` (or startup grace); `GET /readyz` = liveness + DB ping + validated assets | Docker healthcheck and the external watchdog now see the truth; the old bare TCP probe is gone |
| Watchdog | in-process watchdog exits if long polling has been silent for `HEALTH_WATCHDOG_SECONDS` or the DB ping fails 3 checks in a row | Docker restarts a hung bot (kill/restart is deliberate; Telegram buffers updates) |
| Heartbeat | optional `HEARTBEAT_URL` GET every 60 s | external dead-man switch without SSH |
| DB dial/pool (H-5) | `DB_SSLMODE`, `DB_CONNECT_TIMEOUT_SECONDS`, `DB_MAX_CONNS`; connection built as a URL (escapes secrets correctly) | explicit TLS mode, bounded connects, bounded pool |
| Nightly backup safety | `/admin backup` skips files younger than 60 s (in-flight dumps); the nightly job refuses + notifies if the newest dump is older than 2 h | no partial-file sends, no silently stale nightly copy |
| Backup DB name fix (P0-8) | dumps name the database explicitly | no more "dump of the container's leftover `POSTGRES_DB`" empty-dump class |

Exact list: `git log v3.4.0..hardening/v3.4.1 --stat`.

## 1b. Build toolchain / supply chain

- `go.mod` requires **Go 1.26.11**; the Dockerfile builds with `golang:1.26.11-alpine`
  (both pinned by patch). Reason: the v3.4.0 toolchain (1.25.x) and
  `golang.org/x/text` had open standard-library/module advisories; `govulncheck ./...`
  is clean on this branch ("No vulnerabilities found").
- Dependency updates in the same commit: `golang.org/x/text v0.44.0`,
  `golang.org/x/sync v0.25.0` (both indirect).
- CI now runs `govulncheck` blocking, plus `staticcheck`, a real-PostgreSQL
  integration job, `docker build`, `shellcheck`/`bash -n` and compose-config checks.

## 2. New configuration keys (all optional)

| Key | Default | Notes |
| --- | --- | --- |
| `DB_SSLMODE` | `prefer` | one of `disable allow prefer require verify-ca verify-full`; invalid value fails startup |
| `DB_CONNECT_TIMEOUT_SECONDS` | `5` | integer 1–60; per connection attempt |
| `DB_MAX_CONNS` | `20` | integer 1–100; pgx pool ceiling |
| `HEALTH_WATCHDOG_SECONDS` | `300` | integer ≥ 0; 0 disables the polling/DB watchdog |
| `HEARTBEAT_URL` | empty | optional http(s) URL GET every 60 s; empty = disabled |

Plus the compose `env_file: .env` change (H-9) means these and the previously
dropped keys (`BACKUP_CHAT_ID`, `BACKUP_TIME`, `AI_ENABLED`, `DYNSET_PATH`,
`BATCH_*`, `ALBUM_WINDOW_MS`, `CONTINUE_WINDOW_MS`, `TOKEN_KEYS`, `DB_PORT`) now
reach the process without editing `docker-compose.yml`. Full documented list:
[`.env.example`](../../.env.example).

New/changed env/mount expectations:
- `/app/backups` **must exist** in the container (fails closed otherwise). The repo
  compose files mount it; the Dockerfile is expected to create it as a fallback.
- Running the binary outside Docker (`go run ./cmd/bot`) needs `mkdir -p backups`
  in the working directory.
- `DYNSET_PATH` must point at a mounted, writable path in Docker
  (`/app/state/dynamic_settings.json` in the live compose).

## 3. Ops/config artifacts in this release (destination-agnostic, no Arvan)

| Path | What |
| --- | --- |
| `docker-compose.yml` | production compose: `env_file: .env`, `stop_grace_period: 60s`, log rotation, HTTP `/healthz` healthcheck |
| `deploy/prod/docker-compose.live.yml` | sanitized stack of record for the Germany host (project `chevalet`, containers `telegram-bot`/`telegram-bot-db`, state + verified-backups mounts) |
| `deploy/prod/ops/backup-v2.sh` | no `-t`, flock, timeout, temp+rename, size/gzip/footer/table-count checks, sha256 manifest, retention, alerting hook, `--gate` pre-deploy mode |
| `deploy/prod/ops/offsite-push.sh` | rclone (R2/B2) **or** rsync mirror; optional `age`; sha256-verified before success; loud no-destination state; refuses same-filesystem "offsite" |
| `deploy/prod/ops/restore-drill.sh` | throwaway `postgres:16-alpine`, `ON_ERROR_STOP=1`, invariants, PASS/FAIL + duration, auto-destroy, dated record |
| `deploy/prod/ops/restore-v2.sh` | `verify`/`drill`/`prod` (prod: `--approve`, fresh safety dump, bot-only stop, restore to NEW db, verify, rename swap, start+health/polling, printed rollback). **Refuses legacy `--create` dumps** (cluster-level `DROP/CREATE DATABASE` + `\connect` — verified data-loss class: such a dump drops/recreates the live DB when psql is connected to a different target) and, in `prod`, accepts only artifacts with a `chevalet-backup-v2` manifest. Legacy emergency conversion: `docs/operations/RESTORE.md`. |
| `deploy/prod/ops/alert.sh` | Telegram ops alert via env-only `ALERT_TOKEN`/`ALERT_CHAT_ID` (dedicated bot) |
| `deploy/prod/ops/watchdog.sh` | external read-only SSH monitor (container/health/restarts, pg_isready, /healthz+/readyz, backup age, disk, load, polling), state-change alerts + daily digest |
| `deploy/prod/systemd/` | 15-min backup/offsite timers, daily 04:30 drill, 5-min watchdog timer, `OnFailure=` alert template, install notes |
| `.github/workflows/ci.yml` | + PostgreSQL-service integration job (`chevalet_test` + `botit`), staticcheck, govulncheck, docker build, shellcheck, `bash -n`, compose-config validation; deploy job gated on `DEPLOY_ENABLED` until the new-host deploy path exists |
| `deploy/go/*.yml` | staging/cutover stacks get the same env_file/grace/logging/HTTP-probe treatment; old `/opt/chevalet-go-staging` paths marked historical |
| `docs/operations/*.md` | runbooks: incident triage, restore, DR rebuild, deploy/rollback, backup verification, disk-full |

Restore safety note: all artifacts that exist on the production host today
predate this release and are legacy `pg_dump --create` dumps. They are NOT
restorable by `restore-v2.sh` (refused by design); the first `backup-v2.sh` run
(ROLLOUT Phase 1.3) creates the first v2 artifact, and the documented off-host
conversion path covers a legacy dump in an emergency.

Off-host destination options (Atur has **no Arvan access**; nothing depends on
Arvan or the Iran host): (1) rclone to Cloudflare R2 / Backblaze B2 (pending an
account/credentials), (2) an encrypted external archive / workstation mirror via
rsync, (3) the bot's nightly Telegram send as tertiary only — **not counted
toward the RPO**. Monitoring: external watchdog on Atur's workstation +
healthchecks.io dead-man switch.

## 4. Verification evidence

Ops/scripts (run locally during development, disposable Postgres, all cleaned up):

| Check | Result |
| --- | --- |
| `bash -n` on every repo shell script | PASS |
| `shellcheck -S warning` on `deploy/prod/ops/*.sh` + `deploy/go/*.sh` | PASS |
| `docker compose config -q` — root, live, and all four `deploy/go` stacks | PASS |
| `.github/workflows/ci.yml` — actionlint | PASS |
| `backup-v2.sh` vs seeded 8-table DB | `backup ok: … size=2057920B tables=8 users=20000 sha=31709abd…` |
| `restore-drill.sh` | `RESULT: PASS artifact=… tables=8 users=20000 cids=23000 bad_uids=0 dup_cids=0 duration=2s` |
| `restore-v2.sh verify` / `drill` | `VERIFY PASS` / second `PASS` |
| `backup-v2.sh --gate` | `GATE OK: … 0m old`; empty root → exit 1 + alert hook |
| retention (hourly ≤48 h / daily 14 d / weekly 8 w) | exercised: prune + keep-all tiers verified |
| `offsite-push.sh` rsync path (different filesystem) | `1 artifact checksum(s) verified` + independent `sha256sum -c` OK |
| `offsite-push.sh` no destination / age missing | loud ERROR+marker / exit 1 (no false success) |
| failure alerting (missing DB container) | exit 1 + `ALERT` line (alert.sh invoked) |

Repo-wide Go evidence (final run, go1.26.11, real PostgreSQL; full log
`audit/2026-11-24/evidence/test-all-20261125_005034.log` and
`audit/2026-11-24/TEST-REPORT.md`):

| Check | Command | Result |
| --- | --- | --- |
| build | `go build ./...` | **PASS** |
| vet | `go vet ./...` | **PASS** |
| fmt | `gofmt -l $(git ls-files '*.go')` | **PASS** (empty) |
| static analysis | `staticcheck ./...` (2026.2.2) | **PASS** |
| vulnerability scan | `govulncheck ./...` (go1.26.11) | **PASS — "No vulnerabilities found."** |
| tests (race, real DB) | `DB_HOST=… DB_NAME=chevalet_test BOT_TEST_DB=botit go test -race -count=1 ./...` | **PASS 8/8 packages; no DB skips, no races/flakes** |
| coverage | `go test -count=1 -cover ./...` | cmd/bot 47.0% · bot 36.8% (was 17.6) · config 93.2 · db **88.0% (was 2.4)** · dynset 84.6 · encoder 92.2 · health 91.0 · texts 100 |
| container image | `docker build` (`golang:1.26.11-alpine` → `alpine:3.20`) | **PASS** |
| restore-safety regression | legacy `--create` dump vs v2 artifact through `restore-v2.sh` / `restore-drill.sh` | **PASS** — legacy refused (verify exit 1; drill FAIL; prod exit 3, live sentinel intact, no DB created); v2 verify+drill PASS |
| container startup smoke | fake BOT_TOKEN + restored local DB | **PASS** — health→DB→texts order, `/healthz`+`/readyz` 200, exit 1 only at fake-token `getMe`; empty Texts and DYNSET-directory both exit 1 |

Test counts: 3 new adversarial test files / 11 tests + every implementer test file;
the full suite was run with `-race` three times (Lead harness, verifier final,
coverage run) with zero failures. CI runs the same gates (PostgreSQL service job,
staticcheck, govulncheck, docker build, shellcheck, compose config).

## 5. Upgrade procedure (summary)

Full phased, reversible steps: [`deploy/prod/ROLLOUT.md`](../../deploy/prod/ROLLOUT.md).

1. **Pre-flight (read-only)** on the host: capture live compose/inspect/volume
   names; **state-dir migration**:
   `install -d -m 700 /opt/bots/chevalet/state && cp -a dynamic_settings.json state/dynamic_settings.json`
   (keep the old file as `.pre-v3.4.1.bak`); audit `.env` keys.
2. **Backup layer first (zero downtime):** install `deploy/prod/ops/*.sh` +
   systemd timers + `/etc/chevalet/ops.env`; prove one certified artifact, one
   verified off-host copy and a PASS drill before any bot change.
3. **Compose reconcile (window):** install `deploy/prod/docker-compose.live.yml`,
   then recreate the bot only:
   `IMAGE_TAG=<current> docker compose -p chevalet up -d --no-deps telegram-bot`.
   The DB container must keep its uptime.
4. **Deploy the v3.4.1 image** (tag pipeline when restored, else the documented
   manual image path in ROLLOUT Phase 4) after `backup-v2.sh --gate` passes.
5. **Verify:** `healthy`, `/readyz` 200, `bot polling`, restart count 0, smoke test,
   old inline buttons still work, admin settings intact.
6. **Monitoring:** watchdog on the workstation + healthchecks.io; exercise the
   alert path in an approved window.

Downtime: seconds (bot container restart only), DB never restarted. Telegram
buffers updates during the gap.

## 6. Rollback

```sh
cd /opt/bots/chevalet
IMAGE_TAG=<previous-tag> docker compose -p chevalet up -d --no-deps telegram-bot
```

The previous image stays on disk (never prune it before acceptance), the compose
file captured in pre-flight is in `ops-captured/compose/`, and the pre-restore
`dynamic_settings.json` is kept as a backup. Database rollback is only for a bad
data-changing operation: `deploy/prod/ops/restore-v2.sh prod <artifact> --approve`
(returns to any pre-restore artifact; the previous database is kept).

## 7. Known limitations / open items (truthful)

- **Off-host destination not yet configured**: until `RCLONE_REMOTE` or
  `OFFSITE_DIR` + credentials are set, off-host RPO is unmet. `offsite-push.sh`
  reports this loudly and writes a marker; it never claims success.
- **Tag-driven deploy is inactive on the current host**: the forced-command script
  targets the old host. CI deploy is gated by `DEPLOY_ENABLED`; re-establish per
  ROLLOUT Phase 3 before using it.
- **Nightly Telegram backup** needs `BACKUP_CHAT_ID` in the host `.env`; it is
  tertiary only and does not satisfy the RPO.
- **Legacy root scripts** (`backup.sh`, `backup-tiered.sh`, `restore.sh`) are
  superseded but still present; CI only syntax-checks them (removal tracked,
  audit P2-1). Do not use them on the new host.
- **CI actions are pinned to major tags, not commit SHAs** (tracked P2).
- **Watchdog and a long Telegram-only outage:** if Telegram itself is unreachable
  long enough for long polling to be silent beyond `HEALTH_WATCHDOG_SECONDS`
  (default 300 s), the in-process watchdog exits so Docker starts a fresh poll
  loop. This is intentional and harmless for users (Telegram buffers updates for
  up to 24 h), but it means the restart count can rise during a Telegram-side
  outage; the external watchdog is what tells you it is Telegram, not the bot.
  Set `HEALTH_WATCHDOG_SECONDS=0` only if you deliberately want a wedged poll
  loop kept running.
- **Host-only, not exercisable locally:** the systemd timers, off-host sync
  destination, SSH watchdog and the real SIGTERM drain on the production host are
  verified by construction + script self-tests, and must be exercised during
  ROLLOUT phases 1/5 (documented acceptance gates).
- **PITR/WAL archiving is not in this release** (RPO stays 15-min logical dumps).
- Production was **not touched** while preparing this release; every ops script ran
  against disposable local Postgres instances only.
