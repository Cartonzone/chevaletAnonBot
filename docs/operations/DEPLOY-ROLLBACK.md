# RB-04 — Deploy & rollback

Production deploys are **tag-driven** once the deploy path is re-established on the
Germany host (see [`../../deploy/prod/ROLLOUT.md`](../../deploy/prod/ROLLOUT.md)
Phase 3). Until then, ship with the controlled manual image path below. Pushing to
`main` runs tests but never deploys.

**Hard gates for any deploy**

1. `backup-v2.sh --gate` exits 0 (verified artifact < 90 min) — abort otherwise.
2. A fresh off-host copy exists (`offsite-push.sh` verified).
3. Maintenance window 04:00–05:00 Tehran, announced; never during an incident.
4. Previous image still on disk (rollback must not need a rebuild).

## A. Tag-driven deploy (when the deploy path is live)

```sh
git tag -a vX.Y.Z -m "what changed" && git push origin vX.Y.Z
```

CI tests that exact tag, then SSHes with the single forced command `deploy vX.Y.Z`.
The server script checks out the tag, ensures image `chevalet-bot:vX.Y.Z`, recreates
**only** the bot container, waits for `healthy`, waits for a `bot polling` line from
this start, settles 20 s to rule out a crash-loop, and auto-rolls-back on failure.
Log: `/var/log/chevalet-deploy.log`.

If `vars.DEPLOY_ENABLED` is not `true`, the deploy job is skipped by design — set it
only after the host-side script is installed and rehearsed.

## B. Controlled manual deploy (current interim path)

On the workstation, from the reviewed release tag:

```sh
go build ./... && go vet ./... && gofmt -l .          # gofmt must print nothing
go test -race -count=1 ./...
docker build -t chevalet-bot:vX.Y.Z .
docker save chevalet-bot:vX.Y.Z | gzip -9 | ssh -p 9011 root@PROD_HOST 'gunzip | docker load'
```

On the host, after the three gates pass:

```sh
cd /opt/bots/chevalet
/opt/bots/chevalet/ops/backup-v2.sh --gate                    # must exit 0
IMAGE_TAG=vX.Y.Z docker compose -p chevalet up -d --no-deps telegram-bot
docker inspect telegram-bot --format '{{.State.Health.Status}}'   # healthy
docker logs telegram-bot --since "$(docker inspect -f '{{.State.StartedAt}}' telegram-bot)" --tail 50
docker exec telegram-bot-db pg_isready -U root -d mydatabase
docker exec telegram-bot-db psql -U root -d mydatabase -tAc 'select count(*) from users'
```

`--no-deps telegram-bot` is load-bearing: it guarantees the database container is
never recreated by a compose change.

## C. Post-deploy verification (all must hold)

- container `healthy` (HTTP `/healthz`; a pre-v3.4.1 image has no HTTP endpoint and
  will fail this — that is expected only before the v3.4.1 rollout),
- `/readyz` returns 200 (DB ping + assets),
- `bot polling` logged for this start, no `panic:`/`FATAL`,
- restart count stays 0 over ~10 min,
- one real smoke test (send → reply/seen/block) from a test account and an old
  inline button still works,
- user count within ±2 % of pre-deploy,
- admin settings (`/admin` AI url/session, donation) intact.

## D. Rollback (one command, no rebuild)

```sh
cd /opt/bots/chevalet
IMAGE_TAG=<previous-tag> docker compose -p chevalet up -d --no-deps telegram-bot
docker inspect telegram-bot --format '{{.State.Health.Status}}'
docker logs telegram-bot --since "$(docker inspect -f '{{.State.StartedAt}}' telegram-bot)" | grep -c 'bot polling'
```

If the previous image is missing, re-load it (`docker load`) from the release
record; never rebuild a rollback image from an unverified tree. If the bot stays
unhealthy after rollback, follow [`INCIDENT-TRIAGE.md`](INCIDENT-TRIAGE.md) — do not
keep redeploying.

Rolling back code does **not** roll back data. Only a bad data-changing operation
needs [`RESTORE.md`](RESTORE.md) (`restore-v2.sh prod`, guarded).

## E. Common failure modes

| Symptom | Likely cause | Action |
| --- | --- | --- |
| `healthy` never reached | crash-loop on startup validation (config, Dynset/backups dir, DB) | `docker logs`; fix config/mount; rollback if the new image is at fault |
| healthy but no `bot polling` | Telegram unreachable / `PROXY` wrong / second poller (409) | check `PROXY`, ensure only one bot container uses the token; rollback |
| old buttons stop working | `BOT_TOKEN` changed without `TOKEN_KEYS` carrying the old token | restore `TOKEN_KEYS` (old token) and restart; see `MIGRATION.md` |
| admin settings reset | `DYNSET_PATH` not on the state mount | restore `state/dynamic_settings.json`, fix compose, recreate bot |

## F. Local run note

Running the bot binary outside Docker (`go run ./cmd/bot`) now requires the runtime
asset dirs to exist and be writable in the working directory:

```sh
mkdir -p backups
# DYNSET_PATH defaults to ./dynamic_settings.json (parent dir must exist)
go run ./cmd/bot
```
