# Common developer commands. See README.md.
SHELL := /bin/bash
TEST_DATABASE_URL ?= postgres://wallet:wallet@127.0.0.1:$${WALLET_PG_PORT:-5432}/wallet?sslmode=disable
export GOTOOLCHAIN := local

.PHONY: up down logs migrate api worker test test-unit test-race lint flutter-test flutter-analyze docs grant-admin

up:            ## Start postgres, redis, migrations, api and worker
	test -f .env || cp .env.example .env
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f api worker

migrate:       ## Apply migrations against DATABASE_URL
	cd backend && go run ./cmd/walletctl migrate up

api:           ## Run the API locally (needs .env exported)
	cd backend && set -a && source ../.env && set +a && go run ./cmd/api

worker:
	cd backend && set -a && source ../.env && set +a && go run ./cmd/worker

test-unit:     ## Unit tests only (integration tests skip without TEST_DATABASE_URL)
	cd backend && go test ./...

test:          ## Full backend suite incl. PostgreSQL integration tests, with race detector
	cd backend && TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -race -count=1 ./...

lint:
	cd backend && go vet ./... && gofmt -l . | (! grep .)

flutter-analyze:
	cd mobile && flutter analyze

flutter-test:
	cd mobile && flutter test

docs:          ## Serve Swagger UI for backend/api/openapi.yaml on :8081
	docker compose --profile docs up -d docs

grant-admin:   ## make grant-admin PHONE=0712345678 ROLE=admin
	docker compose run --rm --entrypoint /usr/local/bin/walletctl api grant-role $(PHONE) $(or $(ROLE),admin)
