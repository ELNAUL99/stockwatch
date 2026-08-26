# Every target is .PHONY because none of them produce a file of that name.
# Without it, a directory called "test" would make `make test` say
# "nothing to be done" — a genuinely confusing failure.
.PHONY: help test test-all test-integration cover lint vet fmt tidy staticcheck \
        build run migrate migrate-down migrate-status seed seed-fresh \
        up down logs ps docker-build clean ci

# The database the host-side tools talk to. Overridable:
#   make migrate DATABASE_URL=postgres://...
DATABASE_URL ?= postgres://stockwatch:stockwatch@localhost:5432/stockwatch?sslmode=disable
export DATABASE_URL

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

GO_LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

# help is the default target, so a bare `make` explains itself rather than
# running something unexpected. The awk scrapes the ## comments below.
help:
	@echo "stockwatch — replenishment service"
	@echo
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / \
		{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
	@echo
	@echo "DATABASE_URL=$(DATABASE_URL)"

## ---- Testing ---------------------------------------------------------------

test: ## Run unit tests only (fast; skips anything needing Docker)
	go test -short -race ./...

test-all: ## Run every test, including Postgres integration tests (needs Docker)
	go test -race ./...

test-integration: ## Run only the Postgres integration tests (needs Docker)
	go test -race -run 'Test' ./internal/storage/ -v

cover: ## Report coverage on the domain package
	go test -short -coverprofile=coverage.out ./internal/inventory/
	@go tool cover -func=coverage.out | tail -1
	@echo "HTML report: go tool cover -html=coverage.out"

## ---- Code quality ----------------------------------------------------------

lint: vet staticcheck ## Run go vet and staticcheck

vet: ## Run go vet
	go vet ./...

# `go install` puts binaries in GOBIN, or GOPATH/bin when GOBIN is unset — and
# neither is reliably on PATH. Resolving the path here rather than trusting PATH
# means the target works immediately after installing, instead of reinstalling
# on every run because `command -v` cannot see it.
GOBIN := $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN := $(shell go env GOPATH)/bin
endif
STATICCHECK := $(GOBIN)/staticcheck

staticcheck: ## Run staticcheck, installing it on demand
	@test -x "$(STATICCHECK)" || { \
		echo "installing staticcheck into $(GOBIN)..."; \
		go install honnef.co/go/tools/cmd/staticcheck@latest; \
	}
	"$(STATICCHECK)" ./...

fmt: ## Format all Go source
	gofmt -w .

tidy: ## Prune and verify go.mod / go.sum
	go mod tidy
	go mod verify

## ---- Building and running --------------------------------------------------

build: ## Build both binaries into ./bin
	go build -trimpath -ldflags="$(GO_LDFLAGS)" -o bin/stockwatch ./cmd/server
	go build -trimpath -ldflags="$(GO_LDFLAGS)" -o bin/migrate    ./cmd/migrate

run: ## Run the server against a local Postgres (see `make up` first)
	go run ./cmd/server

## ---- Database --------------------------------------------------------------

migrate: ## Apply pending migrations
	go run ./cmd/migrate

migrate-down: ## Roll back the most recent migration (destructive)
	go run ./cmd/migrate -down

migrate-status: ## Show which migrations have been applied
	go run ./cmd/migrate -status

# Extra flags for the seeder. To reproduce the exact figures quoted in the
# README, pin the end date as well as the PRNG seed:
#   make seed SEED_ARGS='-end-date=2026-06-30'
SEED_ARGS ?=

seed: ## Load 21 demo SKUs with 60 days of synthetic sales history
	go run ./cmd/seed $(SEED_ARGS)

seed-fresh: ## Reseed with a different random draw (breaks the README's numbers)
	go run ./cmd/seed -seed=$$RANDOM $(SEED_ARGS)

## ---- Docker ----------------------------------------------------------------

up: ## Start Postgres and the service, waiting until both are healthy
	docker compose up --build --wait

down: ## Stop everything and delete the database volume
	docker compose down -v

logs: ## Tail the service logs
	docker compose logs -f stockwatch

ps: ## Show container status
	docker compose ps

docker-build: ## Build the image without starting anything
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t stockwatch:$(VERSION) -t stockwatch:latest .

## ---- Meta ------------------------------------------------------------------

ci: tidy lint test-all ## Everything CI runs, locally

clean: ## Remove build and coverage artefacts
	rm -rf bin coverage.out
	go clean -testcache
