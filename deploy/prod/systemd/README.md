# systemd units — install notes (no auto-install)

These units run the ops scripts on a schedule instead of cron. Nothing here is
installed automatically; the production rollout is a reviewed step in
[`../ROLLOUT.md`](../ROLLOUT.md).

Why systemd over cron: `Persistent=true` runs a missed job after downtime
(instead of silently skipping it), `OnFailure=` gives a first-class alert hook,
and output goes to the journal (cron failures are silent without an MTA).

## On the production host (Germany)

Prerequisites: the repo is checked out at `/opt/bots/chevalet` and the ops scripts
are installed at `/opt/bots/chevalet/ops/` (mode 0750, root).

```sh
# 1) scripts (from a release checkout; see ROLLOUT.md for the full sequence)
install -d -m 700 /opt/bots/chevalet/ops
install -m 755 /opt/bots/chevalet/deploy/prod/ops/*.sh /opt/bots/chevalet/ops/

# 2) root-only ops env (NO secrets in the repo). Example:
install -d -m 700 /etc/chevalet
cat >/etc/chevalet/ops.env <<'EOF'
# dedicated ops alert bot (NOT @Chevalet_bot, so alerts survive the bot being down)
ALERT_TOKEN=123456:CHANGE_ME
ALERT_CHAT_ID=-1000000000000
# off-host destination — pick ONE (destination-agnostic; no Arvan dependency):
# RCLONE_REMOTE=r2:chevalet-backups
# OFFSITE_DIR=/srv/chevalet-offsite
# AGE_RECIPIENT=age1...            # optional client-side encryption
# HC_PING_URL=https://hc-ping.com/...   # optional dead-man switch
EOF
chmod 600 /etc/chevalet/ops.env

# 3) install + enable the timers
install -m 644 /opt/bots/chevalet/deploy/prod/systemd/chevalet-backup.service \
               /opt/bots/chevalet/deploy/prod/systemd/chevalet-backup.timer \
               /opt/bots/chevalet/deploy/prod/systemd/chevalet-offsite.service \
               /opt/bots/chevalet/deploy/prod/systemd/chevalet-offsite.timer \
               /opt/bots/chevalet/deploy/prod/systemd/chevalet-drill.service \
               /opt/bots/chevalet/deploy/prod/systemd/chevalet-drill.timer \
               /opt/bots/chevalet/deploy/prod/systemd/chevalet-alert@.service \
               /etc/systemd/system/
chmod 644 /etc/systemd/system/chevalet-alert@.service
systemctl daemon-reload
systemctl enable --now chevalet-backup.timer chevalet-offsite.timer chevalet-drill.timer

# 4) prove it
systemctl list-timers --all | grep chevalet
systemctl start chevalet-backup.service && journalctl -u chevalet-backup -n 50 --no-pager
journalctl -u chevalet-offsite -n 30 --no-pager      # must say "verified", or the loud no-destination error
```

Migration from the legacy `backup-tiered.sh` cron: keep the old cron line for a
day alongside the new timer (the 15-min timer runs at :00/:15/:30/:45, so it will
overlap at :00 — the `flock` in `backup-v2.sh` makes that safe), compare the new
artifacts under `backups-v2/published/`, then remove the old cron line. No
container restart is involved.

## On the monitoring host (workstation / second host)

Does not touch production; it only reads over SSH.

```sh
# scripts in a stable path (any path works — edit ExecStart to match)
install -d -m 700 /opt/chevalet/ops
install -m 755 <repo>/deploy/prod/ops/*.sh /opt/chevalet/ops/

# dedicated key for read-only checks (BatchMode; no password login)
#   ~/.ssh/id_chevalet_watchdog  — add its public key to the production
#   authorized_keys (read-only commands are all this script runs)
# /etc/chevalet/ops.env must contain PROD_HOST / PROD_PORT / PROD_KEY / ALERT_*
# and, if used, the SSH known_hosts file:
#   PROD_HOST=181.41.194.149
#   PROD_PORT=9011
#   PROD_USER=root
#   PROD_KEY=/home/atur/.ssh/id_chevalet_watchdog
#   ALERT_TOKEN=...
#   ALERT_CHAT_ID=...
chmod 600 /etc/chevalet/ops.env

install -m 644 <repo>/deploy/prod/systemd/chevalet-watchdog.service \
               <repo>/deploy/prod/systemd/chevalet-watchdog.timer \
               /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now chevalet-watchdog.timer
systemctl start chevalet-watchdog.service; journalctl -u chevalet-watchdog -n 30 --no-pager
```

`watchdog.sh --no-alert` prints/persists a state without sending anything — use
it for a first run while wiring the alert bot.

## Files

| Unit | Runs | Schedule |
| --- | --- | --- |
| `chevalet-backup.service` / `.timer` | [`../ops/backup-v2.sh`](../ops/backup-v2.sh) | every 15 min (`*:0/15`) |
| `chevalet-offsite.service` / `.timer` | [`../ops/offsite-push.sh`](../ops/offsite-push.sh) | every 15 min (`*:5/15`) |
| `chevalet-drill.service` / `.timer` | [`../ops/restore-drill.sh`](../ops/restore-drill.sh) | daily 04:30 |
| `chevalet-watchdog.service` / `.timer` | [`../ops/watchdog.sh`](../ops/watchdog.sh) | every 5 min, monitoring host |
| `chevalet-alert@.service` | [`../ops/alert.sh`](../ops/alert.sh) | `OnFailure=` hook for the units above |
