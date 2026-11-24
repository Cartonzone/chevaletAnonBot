# Production ops — ChevaletAnonBot ("never lose the bot again")

Everything here is **repo-only, reviewed, and rehearsed locally**. Nothing in this
directory is auto-installed; production changes are gated by
[`ROLLOUT.md`](ROLLOUT.md) and Atur's approval. See also
[`../../docs/operations/`](../../docs/operations/) for the incident runbooks.

```
deploy/prod/
├── docker-compose.live.yml     # sanitized stack of record for the Germany host
├── ops/
│   ├── backup-v2.sh            # dump -> verify -> publish -> manifest -> retention
│   ├── offsite-push.sh         # destination-agnostic off-host copy, sha256-proven
│   ├── restore-drill.sh        # restore newest artifact into throwaway Postgres
│   ├── restore-v2.sh           # verify | drill | prod (guarded, new-DB swap)
│   ├── alert.sh                # Telegram ops alert (dedicated bot, env-only secrets)
│   └── watchdog.sh             # external read-only SSH monitor (workstation)
├── systemd/                    # timers for the above + install notes
├── ROLLOUT.md                  # phased, reversible production rollout
└── README.md                   # this file
```

## Architecture (destination-agnostic — no Arvan/Iran-host dependency)

```
 ┌──────────── Germany VPS @ Chevalet production ─────────────────────────────┐
 │ telegram-bot → GET /healthz, /readyz on 127.0.0.1:$HEALTH_PORT              │
 │ telegram-bot-db → backup-v2.sh every 15 min: dump → verify → published/     │
 │                  (size, gzip -t, pg_dump footer, >=8 CREATE TABLE, sha256)  │
 │ published/ → offsite-push.sh every 15 min: age (optional) → rclone/rsync    │
 └───────────┬──────────────────────────────────────────┬─────────────────────┘
             │ RCLONE_REMOTE (R2/B2/any rclone)         │ OFFSITE_DIR (rsync mirror:
             │  private bucket + versioning             │  encrypted external archive,
             ▼                                          ▼  workstation, mounted remote)
   ┌──────────────────────────────┐        ┌───────────────────────────────────┐
   │ #1 off-host copy (object st.) │        │ #2 off-host copy (second medium)  │
   └──────────────────────────────┘        └───────────────────────────────────┘
             ▲
             │ checks every 5 min, READ-ONLY over SSH, alerts on state change
   ┌─────────┴─────────────────────────────────────────┐
   │ Atur's workstation: ops/watchdog.sh + alert bot    │
   │ + healthchecks.io dead-man switch (independent)    │
   └────────────────────────────────────────────────────┘
   Tertiary only: the bot's nightly Telegram DB send (BACKUP_CHAT_ID).
   It does NOT count toward the RPO — see docs/operations/BACKUP-VERIFY.md.
```

**Targets:** RPO ≤ 15 min locally (15-min dumps) and ≤ 30 min off-host (15-min
push); RTO ≤ 45 min rehearsed; detection ≤ 5 min externally. The old failure
(2026-11-23) was ~2 h 48 m blind and lost the writes since the last same-disk
dump.

**Design rules**
1. **3-2-1**: the disk that holds the DB never holds the only copy. Two off-host
   copies on different media/providers (one may be the workstation mirror).
2. **No silent config**: the bot gets its whole `.env` via `env_file`; the CI
   `config-check` job validates every compose stack against `.env.example`.
3. **Verified, not hoped**: an artifact is only "a backup" after `backup-v2.sh`
   publishes it with a sha256 + manifest; the drill proves it restores.
4. **Destination-agnostic**: no provider-specific code paths, no Arvan/Iran
   dependency. Pick a destination today; switch later without touching the bot.
5. **Out-of-band alerting**: alerts use a separate bot/token so an alert still
   arrives when @Chevalet_bot is the thing that is broken.
6. **Never** `docker compose down` the production stack (it would stop the DB);
   the bot alone is stopped with `docker stop telegram-bot`.

## Environment for the ops scripts

Secrets never live in the repo. They go in `/etc/chevalet/ops.env` (root, mode
600), which the systemd units load with `EnvironmentFile=-/etc/chevalet/ops.env`:

```sh
# dedicated ops alert bot — NOT @Chevalet_bot
ALERT_TOKEN=123456:CHANGE_ME
ALERT_CHAT_ID=-1000000000000

# --- off-host destination: set exactly ONE of these ---
# 1) object storage via rclone (recommended primary): Cloudflare R2 or Backblaze B2
#    Create a PRIVATE bucket with object versioning + lifecycle retention and a
#    write-only/append-only credential; `rclone config` as root (config 600).
RCLONE_REMOTE=
# RCLONE_REMOTE=r2:chevalet-backups
# RCLONE_REMOTE=b2:chevalet-backups

# 2) directory mirror via rsync (encrypted external archive, workstation, mounted
#    remote, SFTP mount). MUST be a different filesystem/medium than the DB host:
#    offsite-push.sh refuses to call a same-filesystem path an "off-host copy".
OFFSITE_DIR=

# optional client-side encryption before anything leaves the host (age public key;
# private key stays in the vault/offline — never on the production host)
AGE_RECIPIENT=
# AGE_RECIPIENT=age1...

# optional dead-man switch (healthchecks.io): a missing ping pages Atur
HC_PING_URL=

# paths/overrides (defaults are the production values; see each script header)
# BACKUP_ROOT=/opt/bots/chevalet/backups-v2
# DB_CONTAINER=telegram-bot-db
# DB_NAME=mydatabase
# DB_USER=root
```

`ALERT_TOKEN`/`ALERT_CHAT_ID` are also needed on the **monitoring host** (in its
own `/etc/chevalet/ops.env`, which additionally sets `PROD_HOST`, `PROD_PORT`,
`PROD_KEY`).

## Off-host destination options

Both are supported by the same script (`offsite-push.sh`); choose one per host.

### A. Object storage via rclone (Cloudflare R2 / Backblaze B2) — recommended primary

1. Create a **private** bucket (`chevalet-backups`) and enable object versioning
   (or bucket lock/lifecycle retention, >= 30 days). This gives protection against
   an overwrite/delete by a compromised host credential.
2. Create a **write-only / append-only** API credential scoped to that bucket
   (R2: an API token with Object Read & Write on the bucket is the minimum; B2: an
   application key limited to the bucket). Do not give delete permission if your
   provider supports it.
3. Install rclone on the production host (`apt install rclone` or the official
   binary) and configure the remote as root: `rclone config` →
   `r2`/`b2` remote with the credential; `chmod 600 ~/.config/rclone/rclone.conf`.
4. Set `RCLONE_REMOTE=r2:chevalet-backups` (or `b2:…`) in `/etc/chevalet/ops.env`.
5. Prove: `systemctl start chevalet-offsite.service`, then
   `rclone ls "$RCLONE_REMOTE" | tail` — newest `.sql.gz` + `.sha256` +
   `.manifest.json` present, and the service log says `verified (sha256 …)`.
6. Restore rehearsal from the bucket: `rclone copy "$RCLONE_REMOTE/<artifact>" /tmp/`
   then `restore-v2.sh drill /tmp/<artifact>` on the workstation.

### B. rsync mirror (`OFFSITE_DIR`) — encrypted external archive or workstation

1. Mount/attach a different medium (encrypted external disk, SFTP/sshfs mount, or
   pull from the workstation — see `watchdog.sh` for the read-only direction).
2. `install -d -m 700 <mount>/chevalet-backups` and set `OFFSITE_DIR=…`.
3. Prove: `systemctl start chevalet-offsite.service`; the log must say
   `rsync: N artifact checksum(s) verified`.
4. The script refuses to run if `OFFSITE_DIR` is on the same filesystem as
   `BACKUP_ROOT` — that is not an off-host copy and must not be reported as one.

### C. Encryption (recommended with A or B)

Set `AGE_RECIPIENT` to the `age` public key. Every artifact is encrypted on the
production host before it is pushed; only `*.sql.gz.age` + `.sha256` leave the
host. The private key must be kept in the vault/offline and used with
`age --decrypt` for drills/restores. Install `age` on the production host
(`apt install age`).

### D. Telegram nightly send — tertiary ONLY

The bot can send the newest dump to `BACKUP_CHAT_ID` once a night. It is a
convenience copy (easy to grab from a phone) and is **not** counted toward the
RPO: it is single-destination, rate-limited, and depends on the bot's own token
path. Leave it empty when the primary/secondary copies exist.

## Monitoring (external, out-of-band)

- **Watchdog (`watchdog.sh`)** on Atur's workstation / a second host: every 5 min
  it SSHes read-only to production and checks the bot container (running, health,
  restart count), PostgreSQL `pg_isready`, `/healthz` + `/readyz` (via a
  container-local wget, because the endpoints bind loopback), newest verified
  backup age (< 90 min), disk (< 85%), load, and that the bot logged
  `bot polling` since its start. It alerts only on state change (plus recovery,
  a daily all-green digest, and a periodic reminder for an unchanged failure).
- **Dead-man switch (`HC_PING_URL`)**: paste the healthchecks.io ping URL into
  `/etc/chevalet/ops.env`; configure the check to alert to the same ops Telegram
  chat if pings stop for ~10 min. This covers the case where the *monitoring host
  itself* dies.
- No Arvan/Iran-host dependency in either layer.

## Acceptance gates (before calling the P0 work done)

- [ ] `backup-v2.sh` publishes an artifact every 15 min; manifest + sha256 present;
      `restore-drill.sh` prints `RESULT: PASS` (evidence recorded in the rollout log).
- [ ] `offsite-push.sh` reports a verified off-host copy, on a *different*
      filesystem/provider, and the destination is not writable with the bot's own
      credentials.
- [ ] Killing the backup timer for >90 min raises an alert within 15 min (test in
      a maintenance window).
- [ ] Stopping `telegram-bot` raises a watchdog alert within 5 min (restart it
      afterwards); recovery alert arrives.
- [ ] A restore drill from the OFF-HOST artifact passes on the workstation.
- [ ] Disk ≥ 25% free; kernel/security updates applied (separate approval).
- [ ] Runbooks in `docs/operations/` verified against the real host paths.

## Script quick reference

```sh
sudo BACKUP_ROOT=/opt/bots/chevalet/backups-v2 /opt/bots/chevalet/ops/backup-v2.sh
sudo /opt/bots/chevalet/ops/backup-v2.sh --gate          # pre-deploy gate (exit!=0 => abort deploy)
sudo /opt/bots/chevalet/ops/offsite-push.sh
sudo /opt/bots/chevalet/ops/restore-drill.sh [artifact]
sudo /opt/bots/chevalet/ops/restore-v2.sh verify <artifact>
sudo /opt/bots/chevalet/ops/restore-v2.sh drill  <artifact>
# prod restore is deliberate and guarded:
sudo /opt/bots/chevalet/ops/restore-v2.sh prod <artifact> --approve
/opt/chevalet/ops/watchdog.sh --no-alert                 # first run, no alerts
```

See [`../../docs/operations/`](../../docs/operations/) for what to do when the
alert arrives.
