# AGENTS.md

## Project

MeshSDR: Go web application for Software Defined Radio (SDR) with Mesh capabilities.
Functional specs are not written yet — do not invent features or make design choices; ask.

## Stack

- Go 1.26, module `github.com/yohang/mesh-sdr`
- HTTP: `net/http` + `github.com/go-chi/chi/v5`
- Templates: `github.com/a-h/templ`
- Frontend: htmx 4 (vendored in `internal/web/static/vendor/`), Tailwind CSS v4 (standalone CLI, no Node)
- Database: SQLite via `modernc.org/sqlite` (pure Go, `CGO_ENABLED=0`)
- Queries: `sqlc`; migrations: `goose` (embedded, applied by `meshsdr migrate`)
- Config: `github.com/caarlos0/env/v11`; logging: `log/slog`; CLI: `github.com/spf13/cobra`
- Dev hot reload: `air`
- Static assets and migrations embedded with `embed`

## Layout

```
cmd/meshsdr/            entrypoint
internal/cli/           cobra commands (serve, migrate up|down|status)
internal/config/        env-based config struct
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
```

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
- `internal/wire` is the composition root: a `Wire(...)` function builds the whole object graph from `config.Config`, `*slog.Logger`, `*sql.DB`. CLI commands call it.
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

## 12-factor

- Config only via env vars (`config.Config`), never files or flags for environment-specific values; documented in `.env.example`.
- Logs as event streams to stderr; no log files, no rotation.
- Stateless processes; persistent state only in backing services (SQLite file on the `/data` volume, `DB_PATH`).
- Port binding via `HTTP_ADDR`; app is self-contained (embedded assets/migrations).
- Disposability: fast start, graceful shutdown on SIGTERM/SIGINT.
- Admin tasks as one-off subcommands of the same binary (`meshsdr migrate ...`).
- Build/release/run separated: immutable image, config injected at runtime. Dev/prod parity through the same Dockerfile.
- Dependencies explicitly declared (go.mod, pinned tool/asset versions).

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
- Configuration only through env vars on `config.Config` (no prefix). Document new vars in `.env.example`.
- Logging only with `log/slog`.
- Migrations are not applied at startup; run `meshsdr migrate up`.
- `make lint` and `make test` must pass before committing.
- Dockerfile stages: `base` → `deps` → `dev` (Air) / `build` → `prod` (`gcr.io/distroless/static-debian13:nonroot`).
- `.dockerignore` whitelists: ignore everything, then `!` what the build needs.
