#!/usr/bin/env bash
# Chevalet off-host backup push — destination-agnostic.
#
# Copies the VERIFIED artifacts from backup-v2.sh's published/ directory to an
# off-host destination, and proves the copy with sha256 before recording success.
# Design rule (2026-11-24 audit): destination-agnostic. No provider is hard-coded
# and nothing here depends on Arvan/Iran infrastructure.
#
# Destination selection (in order):
#   1. RCLONE_REMOTE set  -> rclone to that remote+path, e.g.
#        RCLONE_REMOTE="r2:chevalet-backups"   (Cloudflare R2)
#        RCLONE_REMOTE="b2:chevalet-backups"   (Backblaze B2)
#      Recommended: private bucket with object versioning/lifecycle retention, a
#      write-only/append-only credential, and `rclone config` done as root (600).
#   2. OFFSITE_DIR set    -> rsync -a --delete mirror into that directory (an
#      encrypted external archive / mounted remote / workstation mirror).
#   3. neither            -> loud ERROR + exit 0: that state is NOT a silent
#      success, a marker file is written and the runbook says off-host RPO is
#      unmet. (Exit 0 keeps the 15-min timer from flapping; the loud line and
#      marker are what the operator/monitor act on.)
#
# Optional client-side encryption:
#   AGE_RECIPIENT=<age public key>  -> every artifact is encrypted with `age`
#   before it leaves the host; only the .age files are pushed. The private key
#   stays in the vault/offline (never on the production host).
#
# Environment:
#   BACKUP_ROOT=/opt/bots/chevalet/backups-v2
#   RCLONE_REMOTE= | OFFSITE_DIR=
#   AGE_RECIPIENT=                     (optional)
#   ALERT_CMD='bash /opt/bots/chevalet/ops/alert.sh'
#   HC_PING_URL=                       (optional dead-man switch ping)
#   RCLONE_FLAGS=--transfers=2         (optional extra rclone flags)
#
# Exit codes: 0 success / nothing-new / not-configured, 1 failure.
set -Eeuo pipefail
IFS=$'\n\t'
umask 077

SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
BACKUP_ROOT="${BACKUP_ROOT:-/opt/bots/chevalet/backups-v2}"
PUBLISHED="$BACKUP_ROOT/published"
ENC_DIR="$BACKUP_ROOT/encrypted"
STATE_DIR="$BACKUP_ROOT/state"
LOG_DIR="$BACKUP_ROOT/log"
LOG_FILE="$LOG_DIR/offsite.log"
LOCK_FILE="${LOCK_FILE:-$BACKUP_ROOT/.offsite.lock}"
RCLONE_REMOTE="${RCLONE_REMOTE:-}"
OFFSITE_DIR="${OFFSITE_DIR:-}"
AGE_RECIPIENT="${AGE_RECIPIENT:-}"
ALERT_CMD="${ALERT_CMD:-bash ${SELF_DIR}/alert.sh}"
HC_PING_URL="${HC_PING_URL:-}"
RCLONE_FLAGS="${RCLONE_FLAGS:---transfers=2}"
RCLONE_RETRIES=3
RSYNC_TIMEOUT_SECONDS=900
NOT_CONFIGURED_MARKER="$STATE_DIR/OFFSITE-NOT-CONFIGURED"

ALERTED=0

log() {
    printf '%s %s\n' "$(date -u +%FT%TZ)" "$*" | tee -a "$LOG_FILE"
}

run_alert() {
    local message="$1"
    ALERTED=1
    log "ALERT: $message"
    if [ -z "$ALERT_CMD" ]; then
        log "ALERT_CMD not configured — alert only logged"
        return 0
    fi
    local -a alert_cmd
    # IFS is globally newline/tab in these scripts; give read an explicit space
    # IFS so an ALERT_CMD like "bash /path/alert.sh" splits into words.
    IFS=' ' read -r -a alert_cmd <<<"$ALERT_CMD"
    "${alert_cmd[@]}" "$message" || log "ALERT_CMD failed"
    return 0
}

die() {
    log "ERROR: $1"
    run_alert "$1"
    exit 1
}

cleanup() {
    local rc=$?
    set +e
    trap - EXIT
    if [ "$rc" -ne 0 ] && [ "$ALERTED" -eq 0 ]; then
        run_alert "offsite push failed (rc=${rc})"
    fi
    exit "$rc"
}
trap cleanup EXIT

mkdir -p "$PUBLISHED" "$STATE_DIR" "$LOG_DIR"
chmod 700 "$BACKUP_ROOT" "$PUBLISHED" "$STATE_DIR" "$LOG_DIR" 2>/dev/null || true
touch "$LOG_FILE"
chmod 600 "$LOG_FILE" 2>/dev/null || true

# ---------------------------------------------------------------------------
# destination decision
# ---------------------------------------------------------------------------
rm -f -- "$NOT_CONFIGURED_MARKER"
if [ -z "$RCLONE_REMOTE" ] && [ -z "$OFFSITE_DIR" ]; then
    {
        echo "ERROR: no off-host destination configured."
        echo "  Set RCLONE_REMOTE (Cloudflare R2 / Backblaze B2 / any rclone remote)"
        echo "  or OFFSITE_DIR (encrypted external archive / workstation mirror)."
        echo "  Until then the off-host RPO is NOT met and this is NOT a success."
    } >&2
    log "WARNING: no off-host destination configured (RCLONE_REMOTE/OFFSITE_DIR unset) — off-host RPO unmet"
    date -u +%FT%TZ >"$NOT_CONFIGURED_MARKER"
    chmod 600 "$NOT_CONFIGURED_MARKER"
    exit 0
fi

if [ -n "$RCLONE_REMOTE" ] && [ -n "$OFFSITE_DIR" ]; then
    die "both RCLONE_REMOTE and OFFSITE_DIR are set — choose exactly one destination"
fi
if [ -n "$AGE_RECIPIENT" ] && ! command -v age >/dev/null 2>&1; then
    die "AGE_RECIPIENT is set but the 'age' binary is not installed (apt install age; see deploy/prod/README.md)"
fi

# Serialize overlapping timer runs.
exec 9>"$LOCK_FILE"
if ! flock -n 9; then
    log "another offsite push is running; skipping this run"
    exit 0
fi

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------
validate_certified_artifact() {
    # Only certified (verified + sha256) published artifacts may be pushed.
    local artifact="$1" base
    base=$(basename "$artifact")
    if [ ! -s "$artifact" ]; then
        log "skip: empty/missing artifact $artifact"
        return 1
    fi
    if [ ! -s "${artifact}.sha256" ]; then
        log "skip: $base has no .sha256 — not a certified backup-v2 artifact"
        return 1
    fi
    if ! gzip -t "$artifact" 2>/dev/null; then
        log "skip: $base fails gzip -t"
        return 1
    fi
    if ! (cd "$(dirname "$artifact")" && sha256sum -c --status "${base}.sha256"); then
        log "skip: $base fails its sha256 check"
        return 1
    fi
    return 0
}

# List the primary artifact basenames in a directory (plain .sql.gz or .sql.gz.age).
list_artifacts() {
    local dir="$1"
    find "$dir" -maxdepth 1 -type f \
        \( -name 'backup_*.sql.gz' -o -name 'backup_*.sql.gz.age' \) \
        -printf '%f\n' 2>/dev/null | sort
}

prepare_encrypted_copies() {
    # Encrypts every published artifact that does not have a fresh .age copy.
    # Idempotent: an existing .age newer than its source is reused after its
    # checksum file has been validated. Never deletes plaintext (the local drill
    # uses it).
    local artifact base enc
    mkdir -p "$ENC_DIR"
    chmod 700 "$ENC_DIR"
    for artifact in "$PUBLISHED"/backup_*.sql.gz; do
        [ -e "$artifact" ] || continue
        base=$(basename "$artifact")
        enc="$ENC_DIR/${base}.age"
        if [ -s "$enc" ] && [ -s "${enc}.sha256" ] && [ "$enc" -nt "$artifact" ]; then
            if (cd "$ENC_DIR" && sha256sum -c --status "${base}.age.sha256"); then
                cp -f -- "$PUBLISHED/${base}.manifest.json" "$ENC_DIR/${base}.manifest.json" 2>/dev/null || true
                continue
            fi
            log "re-encrypting: cached ${base}.age failed its checksum"
        fi
        if ! age --recipient "$AGE_RECIPIENT" --output "$enc.tmp" "$artifact"; then
            rm -f -- "$enc.tmp"
            die "age encryption failed for $base"
        fi
        chmod 600 "$enc.tmp"
        mv -f -- "$enc.tmp" "$enc"
        (cd "$ENC_DIR" && sha256sum "${base}.age" >"${base}.age.sha256")
        chmod 600 "${enc}.sha256"
        cp -f -- "$PUBLISHED/${base}.manifest.json" "$ENC_DIR/${base}.manifest.json" 2>/dev/null || true
        log "encrypted: ${base} -> encrypted/${base}.age"
    done
    return 0
}

sha256_of() {
    sha256sum "$1" | cut -d' ' -f1
}

rclone_push_incremental() {
    local src_dir="$1" state_file="$2"
    local artifact base f plain
    local -a files_to_push=()
    local pushed=0 failed=0 attempt rc=0

    while IFS= read -r artifact; do
        [ -n "$artifact" ] || continue
        base=$(basename "$artifact")
        if grep -qxF -- "$base" "$state_file" 2>/dev/null; then
            continue
        fi
        # Refuse to push a file that is not certified locally.
        plain="${base%.age}"
        validate_certified_artifact "$src_dir/$plain" || die "refusing to push unverified artifact ${plain}"
        files_to_push+=("$base")
    done < <(list_artifacts "$src_dir")

    if [ "${#files_to_push[@]}" -eq 0 ]; then
        log "rclone: no new artifacts to push to ${RCLONE_REMOTE}"
        return 0
    fi

    : >"$STATE_DIR/rclone-files-from.tmp"
    for f in "${files_to_push[@]}"; do
        printf '%s\n' "$f" >>"$STATE_DIR/rclone-files-from.tmp"
        [ -f "$src_dir/${f}.sha256" ] && printf '%s\n' "${f}.sha256" >>"$STATE_DIR/rclone-files-from.tmp"
        plain="${f%.age}"
        [ -f "$src_dir/${plain}.manifest.json" ] && printf '%s\n' "${plain}.manifest.json" >>"$STATE_DIR/rclone-files-from.tmp"
    done

    for ((attempt = 1; attempt <= RCLONE_RETRIES; attempt++)); do
        # shellcheck disable=SC2086 # RCLONE_FLAGS is an operator-provided flag list, intentionally split
        if timeout --signal=TERM 600 rclone copy "$src_dir" "$RCLONE_REMOTE" \
            --files-from "$STATE_DIR/rclone-files-from.tmp" --checksum $RCLONE_FLAGS 2>&1 | tee -a "$LOG_FILE"; then
            rc=0
            break
        fi
        rc=$?
        log "rclone copy attempt ${attempt}/${RCLONE_RETRIES} failed (rc=${rc}), retrying…"
        sleep $((attempt * 5))
    done
    rm -f -- "$STATE_DIR/rclone-files-from.tmp"
    [ "$rc" -eq 0 ] || die "rclone copy to ${RCLONE_REMOTE} failed after ${RCLONE_RETRIES} attempts"

    # Verify every pushed artifact's sha256 against the remote before recording it.
    for f in "${files_to_push[@]}"; do
        local local_sha remote_sha
        local_sha=$(sha256_of "$src_dir/$f")
        remote_sha=$(rclone hashsum sha256 "${RCLONE_REMOTE%/}/${f}" 2>/dev/null | awk '{print $1}' | head -n 1 || true)
        if [ -z "$remote_sha" ]; then
            # Backend without sha256 support: fall back to rclone's own size+hash check.
            if ! rclone check --one-way "$src_dir" "${RCLONE_REMOTE%/}" --include "/${f}" 2>&1 | tee -a "$LOG_FILE" >/dev/null; then
                failed=$((failed + 1))
                log "ERROR: remote verification (rclone check) failed for $f"
                continue
            fi
            log "verified (rclone check): $f"
        elif [ "$remote_sha" != "$local_sha" ]; then
            failed=$((failed + 1))
            log "ERROR: remote sha256 mismatch for $f (local ${local_sha:0:12}… remote ${remote_sha:0:12}…)"
            continue
        else
            log "verified (sha256 ${local_sha:0:12}…): $f"
        fi
        printf '%s\n' "$f" >>"$state_file"
        pushed=$((pushed + 1))
    done
    [ "$failed" -eq 0 ] || die "${failed} artifact(s) failed remote verification at ${RCLONE_REMOTE}"
    log "rclone: pushed+verified ${pushed} new artifact(s) to ${RCLONE_REMOTE}"
    return 0
}

rsync_mirror() {
    local src_dir="$1" mode_label="$2"
    local dest_dev src_dev sha_file verified=0
    dest_dev=$(stat -c %d "$OFFSITE_DIR")
    src_dev=$(stat -c %d "$BACKUP_ROOT")
    if [ "$dest_dev" = "$src_dev" ]; then
        die "OFFSITE_DIR ${OFFSITE_DIR} is on the SAME filesystem as ${BACKUP_ROOT} — that is not an off-host copy"
    fi
    log "rsync: mirroring ${src_dir} -> ${OFFSITE_DIR} (${mode_label})"
    if ! timeout --signal=TERM "$RSYNC_TIMEOUT_SECONDS" \
        rsync -a --delete --partial --timeout=300 "$src_dir"/ "$OFFSITE_DIR"/ 2>&1 | tee -a "$LOG_FILE"; then
        die "rsync to ${OFFSITE_DIR} failed"
    fi
    # Verify every checksum file on the destination. This is the proof that the
    # bytes on the far side are the bytes backup-v2.sh certified.
    for sha_file in "$OFFSITE_DIR"/backup_*.sha256; do
        [ -e "$sha_file" ] || continue
        if ! (cd "$OFFSITE_DIR" && sha256sum -c --status "$(basename "$sha_file")"); then
            die "destination verification failed for $(basename "$sha_file") in ${OFFSITE_DIR}"
        fi
        verified=$((verified + 1))
    done
    if [ "$verified" -eq 0 ]; then
        die "no checksum files found in ${OFFSITE_DIR} after rsync — refusing to claim a verified off-host copy"
    fi
    log "rsync: ${verified} artifact checksum(s) verified in ${OFFSITE_DIR}"
    return 0
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
if [ -n "$AGE_RECIPIENT" ]; then
    log "offsite push start: root=${BACKUP_ROOT} encryption=age"
else
    log "offsite push start: root=${BACKUP_ROOT} encryption=none"
fi

SRC_DIR="$PUBLISHED"
MODE_LABEL="plaintext"
if [ -n "$AGE_RECIPIENT" ]; then
    prepare_encrypted_copies
    SRC_DIR="$ENC_DIR"
    MODE_LABEL="age-encrypted"
fi

# Refuse to push if there is nothing certified at all (an empty destination would
# otherwise "succeed" and mask a broken backup pipeline).
if [ -z "$(list_artifacts "$SRC_DIR")" ]; then
    die "no artifacts in ${SRC_DIR} to push — backup pipeline is broken or nothing has been published yet"
fi

if [ -n "$RCLONE_REMOTE" ]; then
    dest_id=$(printf '%s' "$RCLONE_REMOTE" | sha256sum | cut -c1-12)
    state_file="$STATE_DIR/offsite-pushed-rclone-${dest_id}.list"
    touch "$state_file"
    chmod 600 "$state_file"
    rclone_push_incremental "$SRC_DIR" "$state_file"
else
    mkdir -p "$OFFSITE_DIR"
    chmod 700 "$OFFSITE_DIR" 2>/dev/null || true
    rsync_mirror "$SRC_DIR" "mode=${MODE_LABEL}"
fi

printf '%s destination=%s mode=%s\n' "$(date -u +%FT%TZ)" "${RCLONE_REMOTE:-$OFFSITE_DIR}" "$MODE_LABEL" >"$STATE_DIR/offsite.status"
chmod 600 "$STATE_DIR/offsite.status"
log "offsite push OK: ${RCLONE_REMOTE:-$OFFSITE_DIR} (${MODE_LABEL})"
if [ -n "$HC_PING_URL" ]; then
    if ! curl -fsS --max-time 10 --retry 2 --retry-connrefused "$HC_PING_URL" >/dev/null 2>&1; then
        log "WARN: dead-man switch ping failed — not failing the push"
    fi
fi
exit 0
