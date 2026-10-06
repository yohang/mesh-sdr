# AGENTS.md

## Project

MeshSDR: Go web application for Software Defined Radio (SDR) with Mesh capabilities: one hub (web UI, API, DB, gateway) and N nodes (SDR devices + DSP), shipped as one binary.

- Product spec: `docs/spec/FEATURE_SPEC.md`, `docs/spec/TECHNICAL_SPEC.md` (stack-agnostic, never edited to fit the code; report inconsistencies as issues).
- Backlog: GitHub issues/milestones/epics on `yohang/mesh-sdr` (Project "MeshSDR"). Ticket keys `[GRID-001]`.
- Decisions: `docs/adr/` (spikes write ADRs as `Proposed`; the owner accepts them).
- Do not invent features or make architecture/design choices; ask.

## Stack

- Go 1.26, module `github.com/yohang/mesh-sdr`
- HTTP: `net/http` + `github.com/go-chi/chi/v5`
- Templates: `github.com/a-h/templ`
- Frontend: htmx 4 (vendored in `internal/web/static/vendor/`), Tailwind CSS v4 (standalone CLI, no Node)
- Database: SQLite via `modernc.org/sqlite` (pure Go, `CGO_ENABLED=0`)
- Queries: `sqlc`; migrations: `goose` (embedded, applied by `meshsdr migrate`)
- Config: TOML files (`github.com/BurntSushi/toml`) + env overrides (`github.com/caarlos0/env/v11`); JSON Schema generated from the config structs (`github.com/invopop/jsonschema`)
- Logging: `log/slog`; CLI: `github.com/spf13/cobra`
- REST API: spec-first OpenAPI (`openapi.yaml` embedded, served at `/api/v1/openapi.json`) + `oapi-codegen` (chi server)
- Auth: Argon2id (`golang.org/x/crypto/argon2`), JWT EdDSA access tokens (`github.com/golang-jwt/jwt/v5`), hand-written session-bound CSRF + `http.CrossOriginProtection`; mail: `github.com/wneessen/go-mail`
- Tests: stdlib `testing` only (no assertion library)
- Dev hot reload: `air`
- Static assets and migrations embedded with `embed`

## Layout

```
cmd/meshsdr/            entrypoint (single binary)
internal/cli/           cobra commands (hub, node, all and their subcommands)
internal/config/        TOML + env config loading, origin tracking, JSON Schema
internal/log/           slog logger factory
internal/db/            SQLite connection, go:generate for sqlc
internal/db/migrations/ goose SQL migrations (embedded)
internal/db/queries/    sqlc queries
internal/db/sqlc/       sqlc output (generated)
internal/http/          chi router, middlewares, server
internal/web/           embedded static assets, go:generate for templ + tailwind
internal/web/templates/ templ components
internal/wire/          composition root (hand-written IoC)
internal/shared/domain/ shared kernel (common VOs, domain error type)
internal/<module>/      one bounded context / module, see Architecture
docs/adr/               architecture decision records
.infra/                 infrastructure files (e.g. .infra/docker/… for files the Docker build/compose needs)
```

Modules (bounded contexts):

- `grid`: nodes, enrollment, internal CA / mTLS, control channel, heartbeat, capabilities, device registry, gateway routes
- `identity`: users, roles, sessions, passwords, invitations, access tokens, CSRF, audit log
- `settings`: DB settings store, config locking/precedence, retention
- `shell`: app shell UI (layout, navigation, theming, static pages)

## Architecture (light DDD)

Code is organized by module, layered inside each module:

```
internal/<module>/
  domain/   aggregates, entities, value objects, domain errors, repository interfaces
  app/      use cases / application services (orchestrate domain + ports)
  infra/    adapters: repositories (sqlc), external systems, hardware
  http/     handlers + templ views for this module
  wire.go   module wiring, once the module is big enough (see Dependency injection)
```

Dependency rule: `http` → `app` → `domain` ← `infra`. `domain` imports only the stdlib and `internal/shared/domain`: no SQL, HTTP, templ, slog, config.

The domain must be fully modeled — no primitive obsession, no anemic structs:

- **Value objects**: immutable, unexported fields, compared by value. Created only through `NewX(...) (X, error)`, which validates and returns a domain error if invalid — an existing VO is always valid. Add a `MustX` only for tests/constants. Expose getters/`String()`, never setters.
- **Aggregates**: unexported fields, built by a constructor enforcing invariants, state changed only through behavior methods that keep invariants and return domain errors. One repository per aggregate root; the interface lives in `domain/`, the implementation in `infra/`. Persistence maps rows ↔ aggregates in `infra/` (rehydration constructor, no validation bypass leaking to callers).
- **Domain errors**: explicit and typed (sentinels or a shared `DomainError` type with a stable code) declared in `domain/`. Checked with `errors.Is`/`errors.As`. Boundaries map them to transport responses (e.g. HTTP status); infra errors are wrapped, never leaked as domain errors.
- Use cases in `app/` take/return domain types or dedicated DTOs; handlers stay thin.

## Dependency injection

- Hand-written IoC, no DI library/codegen. Constructor injection only: dependencies are explicit constructor params, interfaces declared on the consumer side. No globals, no `init()` side effects, no service locator.
- `internal/wire` is the composition root: `Wire(...)` functions build the object graph of each role (hub, node) from its config, `*slog.Logger` and, for the hub, `*sql.DB`. CLI commands call them.
- When volume grows, each module exposes its own `internal/<module>/wire.go` (`Wire(deps) Module`), and the root composes modules.

## Logging

Goal: know everything that goes wrong or not as well as expected, plus debug info, filterable by level and by affected component — without cluttering business code.

- Only `log/slog`. Loggers are injected, scoped at wiring: `logger.With(slog.String("component", "<module>.<layer>.<name>"))` (e.g. `radio.infra.repository`). Never `slog.Default()` in business code.
- Levels:
  - `Debug`: flow details useful for diagnosis (inputs, decisions, external calls).
  - `Info`: lifecycle and significant business events.
  - `Warn`: degraded/unexpected but handled (retries, fallbacks, timeouts, slow ops, invalid external data).
  - `Error`: operation failed and needs attention.
- Errors are wrapped with context going up (`fmt.Errorf("load station %s: %w", id, err)`) and logged **once**, at the boundary (HTTP middleware/handler, CLI command, background worker) — no log-and-return.
- Domain stays log-free; it returns errors. `app`/`infra` log Debug/Warn where useful.
- Use `*Context` variants (`InfoContext`, `LogAttrs(ctx, ...)`) and typed attrs (`slog.String`, `slog.Any("error", err)`); stable snake_case keys.
- Centralize repetitive logging in middlewares/decorators rather than repeating it in each function.

## Configuration

Spec TECHNICAL_SPEC §7.4 is authoritative:

- TOML v1.0 files, declarative only, starting with `schema_version`: `hub.toml`, `node.toml` and `hub.d/*.toml`, `node.d/*.toml` drop-ins (lexical order, recursive table merge). Default dir `/etc/meshsdr`, overridable with `--config-dir` / `MESHSDR_CONFIG_DIR`. Secrets referenced by `{ file = "…" }`.
- Every key can be overridden by an env var: `MESHSDR_` prefix, `__` between nesting levels (`db.dsn` → `MESHSDR_DB__DSN`, `hub.listen` → `MESHSDR_HUB__LISTEN`). Env-set keys are locked like file-set keys, with origin `env:<VAR>`.
- Precedence: env > config files > DB settings > defaults. Config/env keys are read-only (locked) in the UI, which shows their origin (`hub.toml:42`).
- Typed Go structs are the source of truth; JSON Schema (draft 2020-12) is generated from them and published. Validation happens at load; invalid config is a startup error.
- The binary never writes config files (only exception: first-start TLS bootstrap of `all`).

## 12-factor

- Environment-specific values come from config files or `MESHSDR_*` env vars (see Configuration); never hard-coded.
- Logs as event streams to stderr; no log files, no rotation.
- Stateless processes; persistent state only in backing services (hub SQLite file at `db.dsn`, `/var/lib/meshsdr` volume in Docker); nodes hold no persistent state.
- Port binding via `hub.listen` / `node.listen`; app is self-contained (embedded assets/migrations).
- Disposability: fast start, graceful shutdown on SIGTERM/SIGINT.
- Admin tasks as one-off subcommands of the same binary (`meshsdr hub migrate`, `meshsdr hub user …`).
- Build/release/run separated: immutable image, config injected at runtime. Dev/prod parity through the same Dockerfile.
- Dependencies explicitly declared (go.mod, pinned tool/asset versions).

## CLI

One binary; the bare role starts the process, admin tasks are subcommands of the role:

- `meshsdr hub` — start the hub; `meshsdr hub migrate [up|down|status]`; `meshsdr hub user add|remove|reset-password|list|disable|enable|exists`
- `meshsdr node` — start a node; `meshsdr node enroll`
- `meshsdr all` — hub + local node (auto-enrolled over loopback)
- Global flags: `-c/--config-dir`, `--noninteractive`, `--silent`, `--json`, `--debug`

## Commands

Everything runs in Docker; no local Go toolchain required. Run `make help` for the full list.

- `make run` — all-in-one: build, generate, migrate, start the dev stack
- `make clean` — stop the stack, drop volumes (caches) and remove generated files / Air output
- `make up` / `make down` / `make logs [c=<service>]` — dev stack with Air hot reload on http://localhost:3000 (`HTTP_PORT` to change host port)
- `make generate` — `go generate ./...` (templ, sqlc, tailwind)
- `make lint` / `make test`
- `make migrate [cmd=up|down|status]`
- `make migrate-create name=<name>` — new sequential goose SQL migration
- `make vendor [HTMX_VERSION=x.y.z]` — refresh vendored htmx
- `make sh` — shell in dev container
- `make build-prod` — production image (distroless, nonroot)

VS Code: "Reopen in Container" (`.devcontainer/`) attaches to the compose `app` service (Air keeps running). The dev image ships gopls, dlv, golangci-lint and the go.mod tools (templ, sqlc, goose, air) on `PATH`; rebuild the image after bumping tool versions.

Dev containers are rootless: the `dev` stage creates an `app` user with the host UID/GID (`UID`/`GID` build args, exported by the Makefile; the devcontainer remaps it via `updateRemoteUserUID`), so files written to the bind mount belong to the host user. Never run dev commands as root.

## Conventions

- Generated files are never committed nor edited: `*_templ.go`, `internal/db/sqlc/`, `internal/web/static/css/app.css`. Regenerate with `make generate`.
- Go tools are declared with the go.mod `tool` directive (`go get -tool <pkg>`) and run with `go tool <name>`.
- Schema changes go through goose migrations only; sqlc reads schema from `internal/db/migrations/`.
- Configuration only through the config structs in `internal/config` (TOML + `MESHSDR_` env). Document new keys in the sample configs under `.infra/`.
- Logging only with `log/slog`.
- Migrations are not applied at startup; run `meshsdr hub migrate`. The hub refuses to start while migrations are pending.
- `make lint` and `make test` must pass before committing.
- Tests: stdlib `testing`, table-driven; repositories tested against real SQLite in `t.TempDir()` through a shared contract suite (future adapters run the same suite).
- Dependencies: stdlib and `golang.org/x/*` are fine; any other third-party dependency requires the owner's approval.
- REST: every `/api/v1` endpoint is declared in `openapi.yaml` first, then generated; one JSON error format.
- Git: one branch + PR per epic (`epic/<area>-<n>`), split into ordered parts when another epic needs a subset first; PR body lists `Closes #<n>` per ticket; spikes get `spike/<key>-<topic>` branches. No AI attribution in commits or PRs.
- Dockerfile stages: `base` → `deps` → `dev` (Air) / `build` → `prod` (`gcr.io/distroless/static-debian13:nonroot`).
- `.dockerignore` whitelists: ignore everything, then `!` what the build needs.
