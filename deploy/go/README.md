# Staging the Go bot (+ historical cutover files)

Run the Go port against a throwaway Postgres, isolated from production, to
live-test it with a **staging** bot token. Telegram is unreachable without a
proxy, so a working `PROXY` is required.

> **Current production is NOT this stack.** Since 2026-11-24 production runs on the
> Germany host with [`deploy/prod/docker-compose.live.yml`](../prod/docker-compose.live.yml)
> (containers `telegram-bot` / `telegram-bot-db`, project `chevalet`, root
> `/opt/bots/chevalet`). The cutover-era files here
> (`docker-compose.prod.yml`, `docker-compose.cutover.yml`,
> `docker-compose.sharedtest.yml`, `backup.sh`, `nightly-backup-backstop.sh`) are
> **historical** and their `/opt/chevalet-go-staging/...` paths refer to the OLD
> host. Do not use them on the current host.

## Steps (on a server/host that has Docker + proxy access)

1. Copy the example env next to this compose file:

   ```sh
   cp ../../.env.example deploy/go/.env   # or create deploy/go/.env
   ```

2. Edit `deploy/go/.env`:
   - `BOT_TOKEN` = your **staging** bot token (a second bot from BotFather)
   - `PROXY` = your proxy URL, e.g. `socks5://127.0.0.1:1080` or `http://…`
   - `DB_NAME`, `DB_USER`, `DB_PASS` = anything (a fresh test DB is created)
   - the rest can stay at the example defaults for now

3. Bring it up:

   ```sh
   cd deploy/go
   docker compose up --build
   ```

   The bot runs `CREATE TABLE IF NOT EXISTS` on the fresh DB, connects through
   the proxy, and starts long polling.

4. In Telegram, message the staging bot. The full bot is ported, so you can
   exercise everything end-to-end:
   - **Commands:** `/help`, `/privacy`, `/donate`, `/myuid`, `/bug`,
     `/settings`, `/my_links`, `/cancel`, and `/admin help` (from an `ADMINS`
     uid).
   - **Sending:** open the bot via one of your `/my_links` links
     (`t.me/<bot>?start=<cid>`) in a second account and send a message; check
     the **reply / seen / block / report** buttons on the delivered message,
     the "sent with link N" indicator, and the deletion warning + its delete
     button.
   - **Replies:** use the **⌨️ ارسال جواب** button, then also just *reply* to a
     delivered message (auto-reply); reply to a channel post whose bio/pin holds
     a `?start=` link (reply-to-channel).
   - **Settings:** change name, custom/audio tags, toggle wpp / seen / warning,
     unblock-me / unblock-all.
   - **my_links:** add / remove / rename a link (limit + uniqueness checks).
   - **Media groups** (album), **quotes**, and (if `GM_GROUP_ID` is set + an
     `AI_URL` reachable) the **AI reply** in the GM group.

   Each first message also exercises the `prep` middleware + `init_user` (the
   user row, a cid and a chevaletid are created in the test DB).

`.env` is git-ignored — the token never lands in the repo.

## Backups (HISTORICAL section — current procedure is deploy/prod)

This staging-era `backup.sh`/`nightly-backup-backstop.sh` pair belonged to the OLD
host's `/opt/chevalet-go-staging` deploy. Production now uses the verified
15-minute pipeline in [`deploy/prod/ops/`](../prod/ops/):

- `backup-v2.sh` — dump → verify → publish (sha256 + manifest) → retention →
  alert; `--gate` for pre-deploy.
- `offsite-push.sh` — destination-agnostic off-host copy (rclone to R2/B2 or an
  rsync/encrypted mirror), sha256-verified.
- `restore-drill.sh` / `restore-v2.sh` — automated drill and guarded restore.

The bot's nightly Telegram send to `BACKUP_CHAT_ID` still exists
(`internal/bot/background.go`'s `backupLoop`) but is **tertiary only** and is not
counted toward the RPO. See [`docs/operations/BACKUP-VERIFY.md`](../../docs/operations/BACKUP-VERIFY.md)
and [`deploy/prod/README.md`](../prod/README.md).

## Notes

- This stack is for staging/verification only. Production is the Germany host
  (`deploy/prod/`); the historical Python→Go cutover is recorded in
  [`../../MIGRATION.md`](../../MIGRATION.md).
- To verify just the database layer against this Postgres, without Telegram:

  ```sh
  DB_HOST=localhost DB_PORT=5432 DB_NAME=… DB_USER=… DB_PASS=… \
    go test ./internal/db/...
  ```
