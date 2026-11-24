# ChevaletAnonBot

[![CI](https://github.com/aturzone/chevaletAnonBot/actions/workflows/ci.yml/badge.svg)](https://github.com/aturzone/chevaletAnonBot/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/aturzone/chevaletAnonBot)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A fully-featured Telegram bot for sending **anonymous** messages between users —
written in Go (`gotgbot` + `pgx`/PostgreSQL). The bot's interface is in Persian.

> This is the Go implementation: a faithful, line-by-line port of an earlier
> Python bot, verified for behavioral parity before release.

Friend of mine [hamid](https://github.com/zmn-hamid) was not only a programmer but also the initiator of this project, and wherever this project reaches or however much it progresses in the open source world, it is indebted to this person and will always have a contribution to the development of this project.

## How it works

Every Telegram user has a numeric user id. To protect privacy, the bot never
exposes that id. Instead, each user gets a **`chevaletid`** which is encoded with
a custom reversible cipher ([`internal/encoder`](internal/encoder/encoder.go))
before it ever appears anywhere — including the `callback_data` of inline
buttons.

When user **A** sends a message to user **B**, B receives the message carrying
A's *encoded* `chevaletid`. When B replies/reacts, the bot decodes the token,
looks up the real user id in PostgreSQL, performs the action, and re-encodes any
id before exposing it again. The real ids stay in the database; only opaque
tokens travel through Telegram.

> The encoder is a **frozen compatibility contract**: buttons on
> already-delivered messages embed encoded ids, so its output must never change.
> It is locked by an exhaustive golden-vector test.

## Features

- Anonymous messaging via shareable links (`t.me/<bot>?start=<cid>`)
- Multiple links per user, with custom names (`/my_links`)
- Replies by button **or** by simply replying to a delivered message
- Reply-to-channel (post a link in a channel bio/pin and let people reply)
- Per-message actions: **reply, seen, block, report, delete**
- Media: photos, videos, audio, documents, and **albums** (media groups)
- Rich settings (`/settings`): display name, custom/audio tags, web-page-preview,
  seen receipts, deletion warnings, unblock-me / unblock-all, formatting help
- Self-destruct warning + countdown on delivered messages
- Admin panel (`/admin`): stats, ban/unban, mass-message, reports, cid
  management, AI settings, backups
- Optional AI auto-reply in a configured group
- Daily good-morning / good-night greetings (Asia/Tehran)
- HTTP health endpoints (`/healthz`, `/readyz`), graceful shutdown, structured logging

## Tech stack

| Concern | Choice |
| --- | --- |
| Language | Go (see [`go.mod`](go.mod)) |
| Telegram | [`gotgbot/v2`](https://github.com/PaulSonOfLars/gotgbot) (long polling) |
| Database | PostgreSQL via [`pgx/v5`](https://github.com/jackc/pgx) |
| Deploy | Docker + Docker Compose |

## Quick start (Docker)

```sh
git clone https://github.com/aturzone/chevaletAnonBot.git
cd chevaletAnonBot

cp .env.example .env
# Edit .env: set BOT_TOKEN, ADMINS, GM_GROUP_ID, and PROXY if needed.

make up          # build + start the bot and PostgreSQL
make logs        # follow the logs
```

The bot creates its schema automatically (`CREATE TABLE IF NOT EXISTS`) on first
run, so pointing it at an existing database preserves all data.

Other handy targets: `make down`, `make restart`, `make rebuild`, `make backup`,
`make restore FILE=backups/<file>.sql.gz`. Run `make help` for the full list.

## Configuration

All configuration is via environment variables (loaded from `.env`). See
[`.env.example`](.env.example) for the full list. Key ones:

| Variable | Description |
| --- | --- |
| `BOT_TOKEN` | Telegram bot token from [@BotFather](https://t.me/BotFather) |
| `PROXY` | Outbound proxy for Telegram, e.g. `socks5://127.0.0.1:1080` (empty = direct) |
| `ADMINS` | Admin user ids, separated by `\|` |
| `DB_HOST` / `DB_PORT` / `DB_NAME` / `DB_USER` / `DB_PASS` | PostgreSQL connection |
| `REPORT_CHAT_ID` / `ERROR_CHAT_ID` | Chats for reports and error notifications |
| `GM_GROUP_ID` / `GM_GROUP_TOPIC_ID` | Group/topic for greetings & AI replies |
| `AI_URL` / `AI_SESSION_ID` / `AI_INTERVAL` | Optional AI auto-reply endpoint |
| `HEALTH_PORT` | HTTP health port (`/healthz`, `/readyz`, loopback only) |
| `DB_SSLMODE` / `DB_CONNECT_TIMEOUT_SECONDS` / `DB_MAX_CONNS` | DB TLS mode / bounded connect / pool size (v3.4.1) |
| `HEALTH_WATCHDOG_SECONDS` / `HEARTBEAT_URL` | hung-bot watchdog / optional dead-man heartbeat (v3.4.1) |
| `DONATION_LINK` | Link shown by `/donate` |

## Development

Requires Go 1.25+.

```sh
go build ./...
go vet ./...
gofmt -l .            # should print nothing
go test ./...         # encoder golden vectors + DB tests (DB tests skip without DB_HOST)
mkdir -p backups     # runtime asset dir; v3.4.1 fails closed if it is missing
go run ./cmd/bot      # needs a populated .env + reachable PostgreSQL (+ PROXY)
```

Or via the Makefile: `make go-build`, `make go-vet`, `make go-fmt`, `make go-test`.

Project layout:

```
cmd/bot/            main entrypoint
internal/bot/       handlers, conversations, jobs (the bot logic)
internal/db/        PostgreSQL layer (pgx)
internal/encoder/   the chevaletid cipher (+ golden test)
internal/config/    env configuration
internal/texts/     Texts/*.txt loader
internal/health/    HTTP health listener (/healthz, /readyz)
internal/dynset/    dynamic_settings.json (runtime-tunable AI settings)
Texts/              Persian message templates
deploy/go/          isolated staging stack + historical cutover files
deploy/prod/        production stack of record + backup/DR/ops scripts
docs/operations/    incident/restore/DR/deploy/backup/disk runbooks
```

## Staging

To live-test against a throwaway PostgreSQL isolated from production (with a
**staging** bot token), use the stack in [`deploy/go/`](deploy/go/README.md).
The production stack of record is
[`deploy/prod/docker-compose.live.yml`](deploy/prod/docker-compose.live.yml).

## Backups, DR & operations

The 2026-11-23 incident (a dead provider disk + no off-host copy) is why the repo
now ships a rehearsed data layer:

- **Verified backups**: 15-minute dumps checked for size/`gzip -t`/pg_dump
  footer/table count, with sha256 + manifest, published atomically
  ([`deploy/prod/ops/backup-v2.sh`](deploy/prod/ops/backup-v2.sh)).
- **Off-host copies, destination-agnostic**: rclone to Cloudflare R2/Backblaze B2
  or an rsync/encrypted external mirror — no provider lock-in
  ([`deploy/prod/ops/offsite-push.sh`](deploy/prod/ops/offsite-push.sh)).
- **Automated restore drills** into a throwaway Postgres
  ([`deploy/prod/ops/restore-drill.sh`](deploy/prod/ops/restore-drill.sh)), and a
  guarded production restore that keeps the previous database
  ([`deploy/prod/ops/restore-v2.sh`](deploy/prod/ops/restore-v2.sh)).
- **External monitoring**: a read-only SSH watchdog + healthchecks.io dead-man
  switch ([`deploy/prod/ops/watchdog.sh`](deploy/prod/ops/watchdog.sh)).

Rollout (phased, reversible) and the ops-environment reference are in
[`deploy/prod/README.md`](deploy/prod/README.md) and
[`deploy/prod/ROLLOUT.md`](deploy/prod/ROLLOUT.md); failure runbooks live in
[`docs/operations/`](docs/operations/). Release notes:
[`docs/hardening/RELEASE-v3.4.1.md`](docs/hardening/RELEASE-v3.4.1.md).

## Production deployment & operations

Production (`@Chevalet_bot`) runs on the Germany host as containers
`telegram-bot` / `telegram-bot-db` (project `chevalet`, root `/opt/bots/chevalet`).
Deploys are **tag-driven by design** (pushing a `vX.Y.Z` tag ships that exact tag;
pushing to `main` only runs tests), but the server-side deploy path is currently
being re-established on the new host — until then the CI deploy job is gated off
and deploys use the controlled, backed-up manual path.

- Current deploy/rollback procedure:
  [`docs/operations/DEPLOY-ROLLBACK.md`](docs/operations/DEPLOY-ROLLBACK.md) and
  [`deploy/prod/ROLLOUT.md`](deploy/prod/ROLLOUT.md).
- Historical tag-pipeline mechanics (old host): [`deploy/go/RELEASING.md`](deploy/go/RELEASING.md).
- The Python→Go cutover (same schema, no data migration) is recorded in
  [`MIGRATION.md`](MIGRATION.md); it is complete, not a pending operation.

## Contributing

Contributions are welcome! Please read [CONTRIBUTING.md](CONTRIBUTING.md) and the
[Code of Conduct](CODE_OF_CONDUCT.md). For security/privacy issues, see
[SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE) © 2026 aturzone and contributors.
