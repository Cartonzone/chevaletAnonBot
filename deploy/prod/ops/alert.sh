#!/usr/bin/env bash
# Chevalet ops alert sender.
#
# Every ops script calls this through ALERT_CMD, e.g. in the systemd unit:
#   Environment="ALERT_CMD=bash /opt/bots/chevalet/ops/alert.sh"
#   ...
#   "$ALERT_CMD" "backup failed: ..."
#
# SECURITY: no secrets live in this file. The dedicated alert bot credentials are
# read from the environment (ALERT_TOKEN / ALERT_CHAT_ID), which on the host come
# from /etc/chevalet/ops.env (mode 600). Use a SEPARATE bot from @Chevalet_bot so
# an alert still works when the Chevalet bot itself is what is broken.
#
# Usage: alert.sh "<one-line message>"
# Exit:  0 sent, 1 not configured or delivery failed (callers usually use `|| true`
#        so that a failed alert cannot mask the original failure).
set -Eeuo pipefail

usage() {
    echo "usage: alert.sh <message>" >&2
    exit 2
}

[ "$#" -ge 1 ] || usage
msg="$*"
[ -n "${msg// /}" ] || usage

# Keep the alert short: Telegram caps a message at 4096 chars and an ops alert
# should be scannable on a phone.
if [ "${#msg}" -gt 3000 ]; then
    msg="${msg:0:3000}… [truncated]"
fi

if [ -z "${ALERT_TOKEN:-}" ] || [ -z "${ALERT_CHAT_ID:-}" ]; then
    echo "ALERT NOT SENT (ALERT_TOKEN / ALERT_CHAT_ID not configured): ${msg}" >&2
    exit 1
fi

runbook="${ALERT_RUNBOOK_URL:-https://github.com/aturzone/chevaletAnonBot/blob/main/docs/operations/INCIDENT-TRIAGE.md}"
text="⚠️ Chevalet ops alert on $(hostname -s) at $(date -u +%FT%TZ)
${msg}

Runbook: ${runbook}"

# The token appears in curl's argv (Telegram's Bot API only accepts it in the
# URL path), so it is visible to root on this host — acceptable for a root-only
# ops script. It is never echoed into logs: curl errors are reported without it.
set +e
response=$(curl --silent --show-error --fail --max-time "${ALERT_TIMEOUT_SECONDS:-20}" \
    --retry 2 --retry-delay 2 --retry-connrefused \
    -X POST "https://api.telegram.org/bot${ALERT_TOKEN}/sendMessage" \
    --data-urlencode "chat_id=${ALERT_CHAT_ID}" \
    --data-urlencode "text=${text}" \
    --data-urlencode "disable_web_page_preview=true" 2>&1)
rc=$?
set -e

if [ "$rc" -ne 0 ]; then
    echo "ALERT DELIVERY FAILED (curl rc=${rc}): $(printf '%s' "$response" | tr '\n' ' ' | head -c 300)" >&2
    exit 1
fi
case "$response" in
    *'"ok":true'*)
        echo "alert sent"
        exit 0
        ;;
    *)
        echo "ALERT DELIVERY FAILED (unexpected response): $(printf '%s' "$response" | tr '\n' ' ' | head -c 300)" >&2
        exit 1
        ;;
esac
