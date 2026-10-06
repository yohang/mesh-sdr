.DEFAULT_GOAL := help
MAKEFLAGS += --no-print-directory

export UID := $(shell id -u)
export GID := $(shell id -g)

COMPOSE ?= docker compose
RUN     := $(COMPOSE) run --rm --no-deps app
A11Y    := $(COMPOSE) -f .infra/a11y/compose.yaml

HTMX_VERSION ?= 4.0.0
IMAGE        ?= mesh-sdr

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the dev image
	$(COMPOSE) build

.PHONY: up
up: ## Start the dev stack (Air hot reload)
	$(COMPOSE) up -d --build --remove-orphans --wait

.PHONY: down
down: ## Stop the dev stack
	$(COMPOSE) down --remove-orphans

.PHONY: run
run: ## All-in-one: build image, generate, migrate, start the dev stack
	$(MAKE) build
	$(MAKE) migrate
	$(MAKE) up

.PHONY: clean
clean: ## Stop the stack, remove volumes (caches), generated files and Air output
	$(COMPOSE) down -v --remove-orphans
	find internal -name '*_templ.go' -delete -o -name '*.gen.go' -delete
	rm -rf internal/db/sqlite/sqlc internal/http/api/openapi.json internal/web/static/css/app.css internal/web/static/icons tmp

.PHONY: logs
logs: c=app
logs: ## Follow logs (c=<service>, default app)
	$(COMPOSE) logs --tail=100 -f $(c)

.PHONY: sh
sh: ## Open a shell in a dev container
	$(RUN) bash

.PHONY: generate
generate: ## Generate code (templ, sqlc, oapi-codegen, openapi.json) and CSS (tailwind)
	$(RUN) go generate ./...

.PHONY: lint
lint: generate ## Run golangci-lint
	$(RUN) golangci-lint run

.PHONY: test
test: generate ## Run tests
	$(RUN) go test ./...

.PHONY: a11y
a11y: ## Run the accessibility checks (axe-core, CI-only container) against the production image
	$(A11Y) up -d --build --wait hub
	$(A11Y) run --rm --build --no-deps a11y; status=$$?; $(A11Y) down -v; exit $$status

.PHONY: migrate
migrate: generate ## Run hub migrations (cmd=up|down|status, default up)
	$(RUN) go run ./cmd/meshsdr hub migrate $(or $(cmd),up)

.PHONY: migrate-create
migrate-create: ## Create a SQL migration (name=...)
	@test -n "$(name)" || (echo "usage: make migrate-create name=<name>" && exit 1)
	$(RUN) go tool goose -dir internal/db/sqlite/migrations -s create $(name) sql

.PHONY: vendor
vendor: ## Download vendored JS assets (HTMX_VERSION=...)
	$(RUN) curl -fsSL -o internal/web/static/vendor/htmx.min.js https://cdn.jsdelivr.net/npm/htmx.org@$(HTMX_VERSION)/dist/htmx.min.js

.PHONY: build-prod
build-prod: ## Build the production image
	docker build -f .infra/docker/Dockerfile --target prod -t $(IMAGE) .
