SHELL := /bin/zsh

.PHONY: run config-check db-start db-status migrate test vet build check code questions docker-db-up docker-db-down docker-db-reset

run: config-check db-start
	@echo "Starting bot. Press Ctrl+C to stop it."
	go run ./cmd/bot

config-check:
	@test -f .env || (echo "Missing .env. Copy .env.example and fill it first." >&2; exit 1)
	@for key in TELEGRAM_BOT_TOKEN OPENAI_API_KEY DATABASE_URL; do \
		grep -Eq "^$${key}=.+" .env || (echo "Missing $${key} in .env" >&2; exit 1); \
	done

db-start:
	@if pg_isready -q; then \
		echo "PostgreSQL is ready."; \
	else \
		echo "PostgreSQL is not responding. Restarting postgresql@15..."; \
		brew services restart postgresql@15; \
		i=0; \
		while ! pg_isready -q; do \
			i=$$((i + 1)); \
			if [ $$i -ge 15 ]; then \
				echo "PostgreSQL did not become ready in 15 seconds." >&2; \
				exit 1; \
			fi; \
			sleep 1; \
		done; \
		echo "PostgreSQL is ready."; \
	fi

db-status:
	@pg_isready

migrate: config-check db-start
	go run ./cmd/migrate

test:
	go test ./...

vet:
	go vet ./...

build:
	go build ./...

check: test vet build

code: migrate
	go run ./cmd/generate-codes -n 1

questions: migrate
	go run ./cmd/import-questions -file materials/question_bank.csv

docker-db-up:
	docker compose up -d --wait postgres

docker-db-down:
	docker compose down

docker-db-reset:
	docker compose down -v
	docker compose up -d --wait postgres
