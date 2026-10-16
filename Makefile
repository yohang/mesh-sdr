.DEFAULT_GOAL := help
MAKEFLAGS += --no-print-directory

export UID := $(shell id -u)
export GID := $(shell id -g)

COMPOSE ?= docker compose
# -T: no TTY, so targets also work from scripts and CI; RUN_IT keeps one
# for interactive targets.
RUN     := $(COMPOSE) run --rm -T --no-deps app
RUN_IT  := $(COMPOSE) run --rm --no-deps app
A11Y    := $(COMPOSE) -f .infra/a11y/compose.yaml
A11Y_HUB := hub

HTMX_VERSION ?= 4.0.0
# Leaflet (map, ADR 0029): npm tarball version and its SHA-256.
LEAFLET_VERSION ?= 1.9.4
LEAFLET_SHA256  ?= 84c65a256e50657896f54c33bd857b6849ebe94c817803be818bf32a3dde0b77
# Common-password list (ACC-011): SecLists commit and SHA-256 of the list.
SECLISTS_COMMIT ?= 49c3b2d1d2481572bd7b0cb5af875a73cdf9d08e
SECLISTS_SHA256 ?= 1472aafa2561df5e3293aee252aee3ca660c12b399a283cf808bb01b39be388b
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
	$(RUN_IT) bash

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
	trap '$(A11Y) down -v' EXIT; \
	$(A11Y) up -d --build --wait $(A11Y_HUB) && \
	$(A11Y) run --rm -T --build --no-deps a11y

.PHONY: migrate
migrate: generate ## Run hub migrations (cmd=up|down|status, default up)
	$(RUN) go run ./cmd/meshsdr hub migrate $(or $(cmd),up)

.PHONY: migrate-create
migrate-create: ## Create a SQL migration (name=...)
	@test -n "$(name)" || (echo "usage: make migrate-create name=<name>" && exit 1)
	$(RUN) go tool goose -dir internal/db/sqlite/migrations -s create $(name) sql

.PHONY: vendor
vendor: ## Download vendored JS assets (HTMX_VERSION=..., LEAFLET_VERSION=... LEAFLET_SHA256=...)
	$(RUN) curl -fsSL -o internal/web/static/vendor/htmx.min.js https://cdn.jsdelivr.net/npm/htmx.org@$(HTMX_VERSION)/dist/htmx.min.js
	$(RUN) sh -euc '\
		tmp=$$(mktemp -d); trap "rm -rf $$tmp" EXIT; \
		curl -fsSL -o "$$tmp/leaflet.tgz" https://registry.npmjs.org/leaflet/-/leaflet-$(LEAFLET_VERSION).tgz; \
		echo "$(LEAFLET_SHA256)  $$tmp/leaflet.tgz" | sha256sum -c -; \
		tar -xzf "$$tmp/leaflet.tgz" -C "$$tmp"; \
		out=internal/web/static/vendor/leaflet; rm -rf "$$out"; mkdir -p "$$out/images"; \
		cp "$$tmp/package/dist/leaflet.js" "$$tmp/package/dist/leaflet.css" "$$tmp/package/LICENSE" "$$out/"; \
		cp "$$tmp/package/dist/images/"*.png "$$out/images/"'

.PHONY: vendor-passwords
vendor-passwords: ## Download the common-password list (SECLISTS_COMMIT=..., SECLISTS_SHA256=...)
	$(RUN) go run ./internal/identity/infra/commonpw/vendor -commit $(SECLISTS_COMMIT) -sha256 $(SECLISTS_SHA256)

.PHONY: build-prod
build-prod: ## Build the production image (every role; CMD all, or node)
	docker build -f .infra/docker/Dockerfile --target prod -t $(IMAGE) .
