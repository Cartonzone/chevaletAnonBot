# Releasing & rolling back production

> **Current status (v3.4.1 hardening): the tag-driven deploy path is NOT active on
> the current production host.** The forced-command script
> ([`deploy-tag.sh`](deploy-tag.sh)) is the source of truth for that design but it
> targets the OLD host (`/opt/chevalet-go-staging`, project `go`, container
> `chevalet-go-bot-prod`) and has not been adapted/re-installed on the current
> Germany host. CI's deploy job is therefore **gated off**
> (`vars.DEPLOY_ENABLED != 'true'`). Until it is re-established
> ([`../prod/ROLLOUT.md`](../prod/ROLLOUT.md) Phase 3), ship releases with the
> controlled manual image path in
> [`../../docs/operations/DEPLOY-ROLLBACK.md`](../../docs/operations/DEPLOY-ROLLBACK.md).
> Nothing in this file is a claim that the pipeline currently works on the live
> host.

## Current production layout (Germany host)

| Thing | Value |
| --- | --- |
| Bot | `@Chevalet_bot`, container `telegram-bot`, image `chevalet-bot:<tag>` |
| Database | container `telegram-bot-db` (Postgres 16), DB `mydatabase`, user `root` |
| Compose project | `chevalet`, root `/opt/bots/chevalet` |
| Stack of record | [`../prod/docker-compose.live.yml`](../prod/docker-compose.live.yml) |
| Historical old host | `/opt/chevalet-go-staging`, project `go`, container `chevalet-go-bot-prod` (retired) |

Production deploys are **tag-driven by design**: pushing a `vX.Y.Z` tag ships that
exact tag; pushing to `main` only runs the tests. The CI workflow is
[`.github/workflows/ci.yml`](../../.github/workflows/ci.yml); the deploy job runs
only when `DEPLOY_ENABLED=true` is set as a repository variable **and** the ref is
a `v*.*.*` tag or a `workflow_dispatch` with a tag.

## Cut a release (once the deploy path is re-established)

```sh
git checkout main && git pull
git tag -a vX.Y.Z -m "short summary of what changed"
git push origin vX.Y.Z
```

CI runs `build / vet / fmt / test -race` (plus the PostgreSQL integration job,
staticcheck, govulncheck, docker build, shellcheck and compose-config jobs)
against that tag, and only on success runs the deploy job. Versioning is plain
semver and the server-side script refuses anything that is not exactly `vX.Y.Z`.
Before publishing a tag, the release operator must also confirm the ROLLOUT
pre-deploy gate (`backup-v2.sh --gate`) is green on the host.

## Roll back (or redeploy an old version)

Intended mechanism (after the deploy path is live): Actions → **CI** → *Run
workflow* → enter an older tag. Rollback does not rebuild — the server keeps the
last few versioned `chevalet-bot:vX.Y.Z` images, so it re-points at an existing
image.

Interim manual rollback (works today, no deploy path needed):

```sh
cd /opt/bots/chevalet
IMAGE_TAG=<previous-tag> docker compose -p chevalet up -d --no-deps telegram-bot
docker inspect telegram-bot --format '{{.State.Health.Status}}'   # healthy
docker logs telegram-bot --since "$(docker inspect -f '{{.State.StartedAt}}' telegram-bot)" | grep -c 'bot polling'
```

`--no-deps` is required: the database container must never be recreated by a
deploy/rollback. Never `docker compose down` in production.

## How the tag deploy is restricted (design; applies after re-install)

CI sends exactly one string over SSH: `deploy vX.Y.Z`. It does not choose the
command that runs. The deploy key's entry on the server is forced-command
restricted to:

```
command="/usr/local/bin/deploy-chevalet.sh",no-agent-forwarding,no-X11-forwarding,no-port-forwarding,no-pty
```

so whatever a holder of that key asks for lands in `SSH_ORIGINAL_COMMAND` and is
treated as untrusted input. [`deploy-tag.sh`](deploy-tag.sh) extracts a tag only if
the payload matches `^v[0-9]+\.[0-9]+\.[0-9]+$`, never eval's it, and refuses
everything else with exit code 2. A compromised CI token therefore cannot run
arbitrary commands on the box; it can only deploy a tag that already exists in
this repo.

### Installing on the new host (pending, approval-gated)

The adapted script must set the current-host constants (`REPO=/opt/bots/chevalet`,
`COMPOSE_FILE=docker-compose.yml`, `PROJECT=chevalet`, `CONTAINER=telegram-bot`,
`IMAGE=chevalet-bot`, `LOGFILE=/var/log/chevalet-deploy.log`) and:

1. refuse anything but `deploy vX.Y.Z`;
2. `git fetch --tags`, resolve the tag, and check out that exact commit;
3. run the pre-deploy gate (`/opt/bots/chevalet/ops/backup-v2.sh --gate`) and
   abort if it fails;
4. record the running image ID and tag it `chevalet-bot:rollback-<utc>`;
5. `docker compose -p chevalet up -d --no-deps telegram-bot` with `IMAGE_TAG`;
6. wait for the container's `/healthz`-based healthcheck to report `healthy`;
7. wait for `bot polling` from **this** container start, settle 20 s, compare the
   restart count (a TCP-only healthcheck used to report healthy before polling and
   could hide a crash-loop);
8. roll back automatically to the previous image if any gate fails, with a loud
   alert and a non-zero exit so CI goes red;
9. keep the last few versioned images and append to
   `/var/log/chevalet-deploy.log`.

## What the deploy script does NOT do

- It never touches the database container or volume.
- It never restores data (a bad data change needs
  [`docs/operations/RESTORE.md`](../../docs/operations/RESTORE.md)).
- It never deploys a branch — only an existing semver tag.
- It never runs without a fresh verified backup (`backup-v2.sh --gate`).
