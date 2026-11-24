# Makefile for Chevalet Anonymous Telegram Bot
# Usage: make <target>
# Example: make up

.PHONY: help gooz up down restart logs logs-bot logs-db build rebuild status shell db-shell backup backup-tiered backup-setup-auto restore db-reset db-drop-tables clean clean-all clean-all-logs logs-clean update check-health quick-restart deploy emergency-stop ensure-dynset go-build go-test go-test-race go-vet go-fmt go-run go-tidy go-test-integration compose-config shellcheck bash-n ops-backup ops-backup-gate ops-offsite-push ops-restore-drill ops-restore-verify

# Default target - shows help
.DEFAULT_GOAL := help

##@ General

help: ## Show this help message
	@echo "Chevalet Anonymous Telegram Bot - Available Commands:"
	@echo ""
	@echo "When updated the project, do \"make deploy\""
	@echo ""
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Go Development

go-build: ## Compile all packages
	go build ./...

go-test: ## Run the test suite
	go test ./...

go-test-race: ## Run the test suite with the race detector (needs a C compiler)
	go test -race -count=1 ./...

go-vet: ## Run go vet
	go vet ./...

go-fmt: ## Format all Go files in place (gofmt -w)
	gofmt -w .

go-run: ## Run the bot locally (needs a populated .env + reachable PostgreSQL)
	go run ./cmd/bot

go-tidy: ## Tidy go.mod / go.sum
	go mod tidy

##@ Docker Operations

ensure-dynset: ## Create dynamic_settings.json if missing (needed for the bind mount)
	@test -f dynamic_settings.json || (echo '{}' > dynamic_settings.json && echo "created empty dynamic_settings.json")

up: ensure-dynset ## Start all services (production)
	@echo "Starting services..."
	docker compose up -d
	@echo "Services started! Check logs with: make logs"

# NOTE: these `down`/`reset`/`update` targets operate on the LOCAL compose stack
# only. On production NEVER run `docker compose down` (it stops the database) —
# use `docker stop telegram-bot` / the deploy runbooks in docs/operations/.
down: ## Stop all services
	@echo "Stopping services..."
	docker compose down
	@echo "Services stopped."

restart: ## Restart all services
	@echo "Restarting services..."
	docker compose restart
	@echo "Services restarted."

build: ## Build Docker images
	@echo "Building images..."
	docker compose build
	@echo "Build complete."

rebuild: ensure-dynset ## Rebuild and restart services (use after code changes)
	@echo "Rebuilding and restarting..."
	docker compose down
	docker compose up -d --build
	@echo "Rebuild complete! Check logs with: make logs"

##@ Logs & Monitoring

logs: ## Show logs from all services (follow mode)
	docker compose logs -f

logs-bot: ## Show logs from bot only (follow mode)
	docker compose logs -f bot

logs-db: ## Show logs from database only (follow mode)
	docker compose logs -f postgres

logs-tail: ## Show last 100 lines from bot
	docker compose logs --tail=100 bot

status: ## Show status of all containers
	docker compose ps

##@ Development

shell: ## Open bash shell in bot container
	docker compose exec bot bash

db-shell: ## Open PostgreSQL shell
	docker compose exec postgres psql -U root -d mydatabase

db-drop-tables: ## Drop all tables from database (WARNING: Deletes all data but keeps DB!)
	@echo "WARNING: This will drop all tables!"
	@read -p "Are you sure? Type 'yes' to continue: " confirm; \
	if [ "$$confirm" = "yes" ]; then \
		docker compose exec postgres psql -U root -d mydatabase -c "DROP TABLE IF EXISTS users, blocks, cids, reports CASCADE;"; \
		echo "All tables dropped. Restart bot to recreate: make restart"; \
	else \
		echo "Cancelled."; \
	fi

##@ Database Operations

backup: ## Backup database to ./backups/ (simple, 30-day retention)
	@echo "Creating backup..."
	@bash backup.sh
	@echo "Backup complete!"

backup-tiered: ## Backup database with tiered retention (hourly/daily/weekly)
	@echo "Creating backup with tiered retention..."
	@bash backup-tiered.sh
	@echo "Backup complete!"

backup-setup-auto: ## Setup automated hourly backups (Linux/WSL)
	@echo "Setting up automated hourly backups..."
	@bash setup-backup-cron.sh

restore: ## Restore database from backup (Usage: make restore FILE=backups/backup_file.sql.gz)
	@if [ -z "$(FILE)" ]; then \
		echo "Error: Please specify backup file"; \
		echo "Usage: make restore FILE=backups/backup_mydatabase_20250128_120000.sql.gz"; \
		echo ""; \
		echo "Available backups:"; \
		ls -lh ./backups/ 2>/dev/null || echo "No backups found"; \
		exit 1; \
	fi
	@echo "Restoring from $(FILE)..."
	@bash restore.sh $(FILE)

db-reset: ## Reset database (WARNING: Deletes all data! local stack only)
	@echo "WARNING: This will delete all data!"
	@read -p "Are you sure? Type 'yes' to continue: " confirm; \
	if [ "$$confirm" = "yes" ]; then \
		docker compose down -v; \
		docker compose up -d postgres; \
		sleep 5; \
		docker compose up -d bot; \
		echo "Database reset complete."; \
	else \
		echo "Cancelled."; \
	fi

##@ Maintenance

clean: ## Remove stopped containers and unused images
	@echo "Cleaning up Docker resources..."
	docker compose down --remove-orphans
	docker system prune -f
	@echo "Cleanup complete."

clean-all: ## Remove everything including volumes (WARNING: Deletes database! local stack only)
	@echo "WARNING: This will delete the database!"
	@read -p "Are you sure? Type 'yes' to continue: " confirm; \
	if [ "$$confirm" = "yes" ]; then \
		docker compose down -v --remove-orphans; \
		docker system prune -af; \
		echo "All Docker resources removed."; \
	else \
		echo "Cancelled."; \
	fi

logs-clean: ## Remove old log files (older than 30 days)
	@echo "Removing old log files..."
	find ./logs -name "*.log" -mtime +30 -delete 2>/dev/null || true
	find ./backups -name "*.sql.gz" -mtime +60 -delete 2>/dev/null || true
	@echo "Old logs cleaned."

update: ## Pull latest code and rebuild
	@echo "Updating application..."
	docker compose down
	git pull
	docker compose up -d --build
	@echo "Update complete!"

##@ Production ops (deploy/prod) — real scripts, NOT for the local dev stack

# These run the SAME verified scripts production uses. They read their own env
# (DB_CONTAINER/DB_NAME/DB_USER/BACKUP_ROOT/ALERT_CMD/RCLONE_REMOTE/OFFSITE_DIR…)
# and default to the production paths; override for a local test Postgres, e.g.:
#   make ops-backup DB_CONTAINER=my-pg BACKUP_ROOT=$$PWD/.cache/bk
# See deploy/prod/README.md. Full rollout: deploy/prod/ROLLOUT.md.

ops-backup: ## Run the verified backup (dump -> verify -> publish -> retention)
	bash deploy/prod/ops/backup-v2.sh

ops-backup-gate: ## Pre-deploy gate: exit 0 only if a verified artifact is <90 min old
	bash deploy/prod/ops/backup-v2.sh --gate

ops-offsite-push: ## Push the certified artifacts off-host (rclone or OFFSITE_DIR)
	bash deploy/prod/ops/offsite-push.sh

ops-restore-drill: ## Restore the newest artifact into a throwaway Postgres (PASS/FAIL)
	bash deploy/prod/ops/restore-drill.sh

ops-restore-verify: ## Integrity-check an artifact (make ops-restore-verify FILE=path)
	@test -n "$(FILE)" || { echo "usage: make ops-restore-verify FILE=/path/to/backup.sql.gz"; exit 1; }
	bash deploy/prod/ops/restore-v2.sh verify "$(FILE)"

compose-config: ## Validate every compose stack against .env.example
	@set -euo pipefail; 	created=""; 	for f in .env deploy/prod/.env deploy/go/.env deploy/go/.env.prod deploy/go/.env.sharedtest; do 	  if [ ! -e "$$f" ]; then cp .env.example "$$f"; created="$$created $$f"; fi; 	done; 	trap 'rm -f '"$$created"'' EXIT; 	docker compose -f docker-compose.yml config -q; 	docker compose -f deploy/prod/docker-compose.live.yml config -q; 	docker compose -f deploy/go/docker-compose.yml config -q; 	docker compose -f deploy/go/docker-compose.prod.yml config -q; 	docker compose -f deploy/go/docker-compose.cutover.yml config -q; 	docker compose -f deploy/go/docker-compose.sharedtest.yml config -q; 	echo "all compose stacks OK"

shellcheck: ## shellcheck the shipped ops scripts (via the official docker image)
	@command -v docker >/dev/null 2>&1 || { echo "docker is required for make shellcheck"; exit 1; }
	docker run --rm -v "$$PWD:/mnt" -w /mnt koalaman/shellcheck:stable -S warning deploy/prod/ops/*.sh deploy/go/*.sh

bash-n: ## Syntax-check every shell script in the repo (bash -n)
	@set -euo pipefail; 	files=$$(git ls-files '*.sh'); 	[ -n "$$files" ] || { echo "no shell scripts found"; exit 1; }; 	echo "syntax-checking $$(echo "$$files" | wc -l) script(s)"; 	bash -n $$files

go-test-integration: ## Run DB-backed tests against a local Postgres (needs DB_* env)
	@test -n "$${DB_HOST:-}" || { echo "set DB_HOST/DB_PORT/DB_USER/DB_PASS/DB_NAME (DB_NAME must end _test) and BOT_TEST_DB for the e2e tests"; exit 1; }
	go test -race -count=1 ./...

##@ Testing & Debugging

check-health: ## Check health status of all containers
	@docker compose ps --format "table {{.Name}}\t{{.Status}}\t{{.Health}}"

##@ Quick Actions

quick-restart: down up logs-tail ## Quick restart: down, up, show logs

deploy: rebuild logs-tail ## Deploy: rebuild and show logs

emergency-stop: ## Emergency stop (force stop all containers)
	@echo "Emergency stop initiated..."
	docker compose kill
	docker compose down
	@echo "All services forcefully stopped."
