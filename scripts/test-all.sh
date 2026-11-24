#!/usr/bin/env bash
# Full local verification harness for ChevaletAnonBot (hardening/v3.4.1).
#
# Runs the same checks CI runs, plus the DB-backed integration/e2e suites against
# a disposable local PostgreSQL. Safe: the destructive DB tests only run against
# a database whose name ends in `_test` (enforced by internal/db tests).
#
# Usage:
#   scripts/test-all.sh            # full run
#   scripts/test-all.sh unit       # build/vet/fmt/unit only (no DB)
#   scripts/test-all.sh keep       # leave the test Postgres container running
#
# Evidence is appended to audit/2026-11-24/evidence/test-all-<timestamp>.log.
set -Eeuo pipefail
cd "$(dirname "$0")/.."

ROOT=$PWD
MODE=${1:-full}
mkdir -p "$ROOT/.cache" "$ROOT/audit/2026-11-24/evidence"
LOG="$ROOT/audit/2026-11-24/evidence/test-all-$(date +%Y%m%d_%H%M%S).log"

# Go toolchain: keep every cache inside the workspace (the sandbox /tmp is
# per-invocation; /home/atur/go is not writable here).
export GOPATH="$ROOT/.cache/gopath"
export GOMODCACHE="$ROOT/.cache/gomodcache"
export GOCACHE="$ROOT/.cache/gocache"
# Docker CLI needs a writable config dir in this sandbox (the default ~/.docker is read-only).
export DOCKER_CONFIG="$ROOT/.cache/docker"
mkdir -p "$DOCKER_CONFIG"
# staticcheck/govulncheck caches (sandbox: $HOME is read-only; tools live in .cache/bin).
export XDG_CACHE_HOME="$ROOT/.cache/xdg"
mkdir -p "$XDG_CACHE_HOME"
export PATH="$ROOT/.cache/bin:$PATH"

# Local test database. DB_NAME MUST end in _test: the destructive integration
# tests drop tables and refuse anything else.
PG_CONTAINER=chevalet-test-pg
export DB_HOST=127.0.0.1 DB_PORT=55432 DB_USER=root DB_PASS=testpass
export DB_NAME=chevalet_test BOT_TEST_DB=botit

PASS=0 FAIL=0 SKIP=0
section() { printf '\n===== %s =====\n' "$*" | tee -a "$LOG"; }
ok()   { PASS=$((PASS+1)); printf 'PASS  %s\n' "$*" | tee -a "$LOG"; }
bad()  { FAIL=$((FAIL+1)); printf 'FAIL  %s\n' "$*" | tee -a "$LOG"; }
skip() { SKIP=$((SKIP+1)); printf 'SKIP  %s\n' "$*" | tee -a "$LOG"; }

run_step() { # name -- command...
  local name=$1; shift
  section "$name"
  if "$@" 2>&1 | tee -a "$LOG"; then ok "$name"; else bad "$name"; fi
}

# --- local Postgres -----------------------------------------------------------
start_pg() {
  if ! docker inspect "$PG_CONTAINER" >/dev/null 2>&1; then
    section "start local Postgres ($PG_CONTAINER on :55432)"
    docker run -d --name "$PG_CONTAINER" \
      -e POSTGRES_USER="$DB_USER" -e POSTGRES_PASSWORD="$DB_PASS" \
      -e POSTGRES_DB="$DB_NAME" -p 127.0.0.1:55432:5432 postgres:16-alpine >>"$LOG" 2>&1
  else
    docker start "$PG_CONTAINER" >>"$LOG" 2>&1 || true
  fi
  for _ in $(seq 1 60); do
    docker exec "$PG_CONTAINER" pg_isready -U "$DB_USER" >/dev/null 2>&1 && break
    sleep 1
  done
  docker exec "$PG_CONTAINER" pg_isready -U "$DB_USER" >/dev/null 2>&1 || { bad "local Postgres not ready"; exit 1; }
  # e2e (fake Telegram + real DB) needs a DB separate from the destructive one.
  docker exec "$PG_CONTAINER" createdb -U "$DB_USER" "$BOT_TEST_DB" >/dev/null 2>&1 || true
  ok "local Postgres ready"
}

trap 'if [ "$MODE" != "keep" ]; then docker rm -f "$PG_CONTAINER" >/dev/null 2>&1 || true; fi' EXIT

# --- checks -------------------------------------------------------------------
run_step "go build ./..."  go build ./...
run_step "go vet ./..."    go vet ./...

section "gofmt (tracked .go files)"
UNFORMATTED=$(gofmt -l $(git ls-files '*.go'))
if [ -z "$UNFORMATTED" ]; then ok "gofmt"; else printf '%s\n' "$UNFORMATTED" | tee -a "$LOG"; bad "gofmt"; fi

if command -v staticcheck >/dev/null 2>&1; then
  run_step "staticcheck ./..." staticcheck ./...
elif [ -x "$ROOT/.cache/bin/staticcheck" ]; then
  run_step "staticcheck ./..." "$ROOT/.cache/bin/staticcheck" ./...
else
  skip "staticcheck not installed (run: go install honnef.co/go/tools/cmd/staticcheck@latest)"
fi

if command -v govulncheck >/dev/null 2>&1; then
  run_step "govulncheck ./..." govulncheck ./...
elif [ -x "$ROOT/.cache/bin/govulncheck" ]; then
  run_step "govulncheck ./..." "$ROOT/.cache/bin/govulncheck" ./...
else
  skip "govulncheck not installed (run: GOBIN=$ROOT/.cache/bin go install golang.org/x/vuln/cmd/govulncheck@latest)"
fi

if [ "$MODE" = "unit" ]; then
  run_step "unit tests (no DB)" env -u DB_HOST -u DB_PORT -u DB_NAME -u BOT_TEST_DB go test -race -count=1 ./...
  exit $FAIL
fi

start_pg

section "shell scripts: bash -n"
SH_FILES=$(git ls-files '*.sh')$(printf '\n')$(git ls-files 'deploy/prod/ops/*.sh' 'deploy/prod/systemd/*' 2>/dev/null || true)
BAD_SYNTAX=0
while IFS= read -r f; do
  [ -n "$f" ] || continue
  [ -f "$f" ] || continue
  if bash -n "$f" 2>>"$LOG"; then :; else printf 'bash -n failed: %s\n' "$f" | tee -a "$LOG"; BAD_SYNTAX=1; fi
done <<<"$SH_FILES"
if [ "$BAD_SYNTAX" -eq 0 ]; then ok "bash -n"; else bad "bash -n"; fi

# Match CI: shellcheck gates the NEW ops scripts + staging scripts; the legacy
# root backup/restore scripts are only syntax-checked (superseded, P2 cleanup).
SHELLCHECK_FILES="$(git ls-files 'deploy/prod/ops/*.sh' 'deploy/go/*.sh' 2>/dev/null)"
if command -v shellcheck >/dev/null 2>&1; then
  run_step "shellcheck" shellcheck -S warning -x $SHELLCHECK_FILES
elif docker image inspect koalaman/shellcheck:stable >/dev/null 2>&1; then
  run_step "shellcheck (docker)" docker run --rm -v "$ROOT:/mnt" -w /mnt koalaman/shellcheck:stable -S warning -x $SHELLCHECK_FILES
else
  skip "shellcheck not installed"
fi

# --- full test suite with real Postgres --------------------------------------
run_step "go test -race -count=1 ./... (real DB)" go test -race -count=1 ./...

section "coverage (real DB)"
if go test -count=1 -coverprofile="$ROOT/.cache/cover.out" ./... 2>&1 | tee -a "$LOG"; then
  go tool cover -func="$ROOT/.cache/cover.out" 2>/dev/null | tail -1 | tee -a "$LOG" || true
  ok "coverage"
else
  bad "coverage"
fi

# --- container build ----------------------------------------------------------
run_step "docker build" docker build --network=host -t chevalet-ci:local .

section "SUMMARY"
printf 'pass=%d fail=%d skip=%d\n' "$PASS" "$FAIL" "$SKIP" | tee -a "$LOG"
printf 'log: %s\n' "$LOG"
[ "$FAIL" -eq 0 ]
