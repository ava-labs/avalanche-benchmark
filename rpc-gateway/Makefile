DATABASE_URL ?= postgres://rpc_gateway:rpc_gateway@localhost:5432/rpc_gateway?sslmode=disable

.PHONY: db-up db-down migrate keygen seed-demo seed-bombard run build

db-up:
	docker compose up -d

db-down:
	docker compose down

migrate:
	psql "$(DATABASE_URL)" -f migrations/001_init.sql

keygen:
	go run ./cmd/rpc-gateway keygen

seed-demo:
	@test -n "$(RAW_KEY)" || (echo "RAW_KEY is required" >&2; exit 1)
	DATABASE_URL="$(DATABASE_URL)" ./scripts/seed-policy.sh demo "$(RAW_KEY)"

seed-bombard:
	@test -n "$(RAW_KEY)" || (echo "RAW_KEY is required" >&2; exit 1)
	DATABASE_URL="$(DATABASE_URL)" ./scripts/seed-policy.sh bombard "$(RAW_KEY)"

run:
	DATABASE_URL="$(DATABASE_URL)" go run ./cmd/rpc-gateway

build:
	go build ./...
