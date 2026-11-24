# RB-01 — Incident triage ("the bot is down / users get no replies")

Production (replace placeholders): host `PROD_HOST` (Germany VPS, SSH port `9011`),
root `/opt/bots/chevalet`, project `chevalet`, containers `telegram-bot` /
`telegram-bot-db`. **Read-only first** — do not restart anything until the state is
captured.

## 1. Know what is broken before touching anything (≤5 min)

1. Check the last alert: watchdog message / healthchecks.io mail / Telegram ops
   chat. It names the failing check(s).
2. On the host:
   ```sh
   docker ps --format '{{.Names}}\t{{.Status}}' | grep telegram
   docker inspect telegram-bot --format 'running={{.State.Running}} restarts={{.RestartCount}} health={{.State.Health.Status}} started={{.State.StartedAt}}'
   docker logs telegram-bot --since "$(docker inspect -f '{{.State.StartedAt}}' telegram-bot)" 2>&1 | tail -100
   ```
3. Probe the real health endpoints (they bind loopback):
   ```sh
   docker exec telegram-bot wget -qO- http://127.0.0.1:12564/healthz; echo
   docker exec telegram-bot wget -qO- http://127.0.0.1:12564/readyz; echo
   ```
   `/healthz` = process + poll/dispatch progress; `/readyz` = DB ping, assets,
   backups dir. An `nc` port-check is **not** a health check.
4. Database:
   ```sh
   docker exec telegram-bot-db pg_isready -U root -d mydatabase
   docker exec telegram-bot-db psql -U root -d mydatabase -tAc 'select 1'
   docker exec telegram-bot-db psql -U root -d mydatabase -tAc 'select count(*) from users'
   ```
5. Host: `df -h /`, `df -i /`, `cat /proc/loadavg`, `dmesg -T | grep -iE 'error|i/o' | tail`,
   `journalctl --since '-30 min' -p err --no-pager | tail`. See
   [`DISK-FULL.md`](DISK-FULL.md) if disk or I/O is involved.

## 2. Decide the action

| State | Action |
| --- | --- |
| Container running+healthy, DB answers, but no replies / no `bot polling` line | Capture logs, then `docker restart telegram-bot` and watch `/healthz` + logs. If it recurs, collect a goroutine dump/`docker inspect`, do **not** keep restarting blindly. |
| Container restart-looping (restarts increasing) | Read the last 50 lines: startup validation? config? DB auth? Fix the cause, then start. Do **not** delete data volumes. |
| DB down/unreachable | Do **not** `docker compose down`. `docker restart telegram-bot-db` only if the host is healthy and a fresh verified backup exists (`ops/backup-v2.sh --gate`); otherwise leave it for the restore path ([`RESTORE.md`](RESTORE.md)). |
| Host unreachable (SSH fails) | Open the provider console/ticket; the watchdog keeps alerting. Prepare the DR rebuild ([`DR-HOST-REBUILD.md`](DR-HOST-REBUILD.md)). |
| Disk full / I/O errors | [`DISK-FULL.md`](DISK-FULL.md). |

## 3. Evidence collection (before any destructive step)

```sh
mkdir -p /root/incidents/$(date -u +%Y%m%dT%H%M%SZ) && cd $_
docker inspect telegram-bot telegram-bot-db > inspect.json
docker logs telegram-bot --tail 5000 > bot.log 2>&1
docker logs telegram-bot-db --tail 5000 > db.log 2>&1
docker stats --no-stream > stats.txt
df -h > df.txt; cat /proc/loadavg > load.txt; free -m > mem.txt
```

## 4. Recovery verification (all must hold)

- `docker inspect telegram-bot` → running, `healthy`, restart count stable.
- `/healthz` AND `/readyz` return 200.
- Logs show `bot polling` for **this** container start and no new `panic`/FATAL.
- DB `select count(*) from users` matches the pre-incident count (± a few rows).
- One real smoke test from a test account: send → reply/seen/block.
- Error channel quiet (no repeated tracking-code floods).

## 5. After

Record timeline/cause in the brain journal (`bin/brain journal`), add the concrete
symptom to this runbook if it was new, and open a task for any code/config fix.
