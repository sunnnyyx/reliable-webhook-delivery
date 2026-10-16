SHELL := /bin/sh
GO ?= go
GOFMT = $(shell $(GO) env GOROOT)/bin/gofmt
-include .env
export
unexport GOFMT
.PHONY: dev-up dev-down migrate-up migrate-down run-api run-worker test test-race test-integration test-smoke lint fmt build

dev-up:
	docker compose --env-file .env -f deploy/compose.yaml up -d --wait

dev-down:
	docker compose --env-file .env -f deploy/compose.yaml --profile rate-limit --profile observability down

migrate-up:
	$(GO) run ./cmd/migrate up

migrate-down:
	$(GO) run ./cmd/migrate down

run-api:
	$(GO) run ./cmd/api

run-worker:
	$(GO) run ./cmd/worker

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

test-integration:
	@test -n "$(INTEGRATION_DATABASE_URL)" || (echo "Set INTEGRATION_DATABASE_URL to a disposable database"; exit 1)
	@test -n "$(INTEGRATION_REDIS_URL)" || (echo "Set INTEGRATION_REDIS_URL to the local test Redis instance"; exit 1)
	$(GO) test -tags integration -count=1 ./tests/integration

lint:
	@test -z "$$("$(GOFMT)" -l cmd internal migrations tests)" || (echo "Run make fmt to fix formatting"; exit 1)
	$(GO) vet ./...

fmt:
	"$(GOFMT)" -w cmd internal migrations tests

build:
	mkdir -p bin
	$(GO) build -o bin/api ./cmd/api
	$(GO) build -o bin/worker ./cmd/worker
	$(GO) build -o bin/migrate ./cmd/migrate
	$(GO) build -o bin/admin ./cmd/admin
	$(GO) build -o bin/receiver ./cmd/receiver
	$(GO) build -o bin/console ./cmd/console

# Requires Python 3 and a migrated development PostgreSQL instance.
test-smoke: build
	python3 scripts/smoke-skeleton.py

# Example: make admin ARGS='tenant-create --name Demo'
.PHONY: admin
admin:
	$(GO) run ./cmd/admin $(ARGS)

.PHONY: test-accounts-smoke
test-accounts-smoke: build
	python3 scripts/smoke-accounts.py

.PHONY: test-ingestion-smoke
test-ingestion-smoke: build
	python3 scripts/smoke-ingestion.py

.PHONY: run-receiver
run-receiver:
	$(GO) run ./cmd/receiver

.PHONY: test-worker-smoke
test-worker-smoke: build
	python3 scripts/smoke-worker.py

.PHONY: migrate-receiver-up migrate-receiver-down test-replay-smoke
migrate-receiver-up:
	$(GO) run ./cmd/migrate receiver-up

migrate-receiver-down:
	$(GO) run ./cmd/migrate receiver-down

test-replay-smoke: build
	python3 scripts/smoke-replay.py

.PHONY: dev-rate-up
dev-rate-up:
	docker compose --env-file .env -f deploy/compose.yaml --profile rate-limit up -d --wait

.PHONY: test-rate-limit-smoke
test-rate-limit-smoke: build
	python3 scripts/smoke-rate-limit.py

.PHONY: dev-observe-up
dev-observe-up:
	python3 scripts/observability-assets.py
	docker compose --env-file .env -f deploy/compose.yaml --profile rate-limit --profile observability up -d

.PHONY: test-observability-smoke
test-observability-smoke: build
	python3 scripts/smoke-observability.py

.PHONY: run-console test-console-smoke
run-console:
	$(GO) run ./cmd/console

test-console-smoke: build
	python3 scripts/smoke-console.py

# Creates/discards synthetic schemas; output directory must not already exist.
BENCHMARK_OUTPUT ?= benchmarks/local-$(shell date +%Y%m%d-%H%M%S)
.PHONY: benchmark benchmark-plans benchmark-profile security-check
benchmark: build
	python3 scripts/benchmark.py --output "$(BENCHMARK_OUTPUT)" --duration 10 --repetitions 2

benchmark-plans: build
	python3 scripts/benchmark.py --output "$(BENCHMARK_OUTPUT)" --plans-only

benchmark-profile:
	mkdir -p benchmarks/profiles
	$(GO) test ./internal/events -run '^$$' -bench BenchmarkAcceptancePayload -benchmem -count=3 -benchtime=1s -cpuprofile=benchmarks/profiles/acceptance.cpu.pprof -memprofile=benchmarks/profiles/acceptance.heap.pprof -o /tmp/relayforge-events.test
	$(GO) tool pprof -top /tmp/relayforge-events.test benchmarks/profiles/acceptance.cpu.pprof

security-check:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

# Launch existing binaries without Go, module downloads, or build-cache access.
# After changing source, rebuild explicitly with a working Go installation.
.PHONY: local-api local-worker local-console local-receiver local-migrate local-admin
local-api local-worker local-console local-receiver:
	@test -x ./bin/$(patsubst local-%,%,$@) || (echo "Missing executable; run make build with a working Go installation first."; exit 1)
	./bin/$(patsubst local-%,%,$@)

local-migrate:
	@test -x ./bin/migrate || (echo "Missing executable; run make build with a working Go installation first."; exit 1)
	./bin/migrate up

local-admin:
	@test -x ./bin/admin || (echo "Missing executable; run make build with a working Go installation first."; exit 1)
	./bin/admin $(ARGS)

# Optional UI behavior checks use Node's built-in test runner (no npm dependencies).
.PHONY: test-console-ui
test-console-ui:
	node --test internal/console/ui.test.cjs
