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
- Queries: `sqlc`; migrations: `goose` (embedded, per dialect, checksummed, applied by `meshsdr hub migrate`)
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
internal/db/            DB engine contract (Adapter, Migrator, DSN), see docs/adr/0006
internal/db/sqlite/     SQLite dialect adapter (single writer + read pool), sqlc.yaml, go:generate for sqlc
internal/db/sqlite/migrations/ goose SQL migrations of the dialect (embedded)
internal/db/sqlite/queries/    sqlc queries of the dialect
internal/db/sqlite/sqlc/       sqlc output (generated)
internal/db/dbtest/     contract-test harness (engine contract, migrated test DBs)
internal/http/          chi router, middlewares, server
internal/http/api/      openapi.yaml (source of truth), oapi-codegen config, generated server, /api/v1 handlers
internal/http/problem/  RFC 9457 problem+json errors, domain error → HTTP status
internal/web/           embedded static assets (tokens CSS, ES modules, vendored htmx), go:generate for templ + icons + tailwind
internal/web/layout/    app shell templates (document, #main, error page), shared by every module
internal/web/render/    render helper: full page vs htmx fragment, HTML headers, shell error pages
internal/web/icongen/   icon generator (go:generate, golang.org/x/image/vector)
internal/wire/          composition root (hand-written IoC)
internal/shared/domain/ shared kernel (common VOs, domain error type)
internal/<module>/      one bounded context / module, see Architecture
docs/adr/               architecture decision records
.infra/                 infrastructure files
.infra/docker/          Dockerfile (+ Dockerfile.dockerignore), dev/air.toml, dev/config/ (dev hub.toml, node.toml), prod/etc/meshsdr/ (image configs)
.infra/config/          documented sample configs (hub.toml.example, node.toml.example)
.infra/a11y/            CI-only accessibility checker (axe-core + Playwright container, urls.txt, compose.yaml)
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
  infra/    adapters: repositories (infra/<dialect>/, sqlc), external systems, hardware
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
- `internal/wire` is the composition root: `wire.Hub(cfg, logger, adapter)` and `wire.Node(cfg, logger, now)` build the object graph of each role from its config, `*slog.Logger` and, for the hub, the `db.Adapter` opened by `wire.OpenDB` (chosen by the `db.dsn` scheme). CLI commands call them.
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
- Typed Go structs are the source of truth; JSON Schema (draft 2020-12) is generated from them (`meshsdr hub|node config schema`, attached to releases by CI). Validation happens at load; invalid config is a startup error (exit code 78). `meshsdr hub|node config check` validates and prints each key's origin. See docs/adr/0005.
- The binary never writes config files (only exception: first-start TLS bootstrap of `all`).

## 12-factor

- Environment-specific values come from config files or `MESHSDR_*` env vars (see Configuration); never hard-coded.
- Logs as event streams to stderr; no log files, no rotation.
- Stateless processes; persistent state only in backing services (hub SQLite file at `db.dsn`, `/var/lib/meshsdr` volume in Docker); nodes hold no persistent state.
- Port binding via `hub.listen` (default `0.0.0.0:8073`) / `node.listen` (default `0.0.0.0:8074`); app is self-contained (embedded assets/migrations/OpenAPI document).
- Disposability: fast start, graceful shutdown on SIGTERM/SIGINT.
- Admin tasks as one-off subcommands of the same binary (`meshsdr hub migrate`, `meshsdr hub user …`).
- Build/release/run separated: immutable image, config injected at runtime. Dev/prod parity through the same Dockerfile.
- Dependencies explicitly declared (go.mod, pinned tool/asset versions).

## CLI

One binary; the bare role starts the process, admin tasks are subcommands of the role:

- `meshsdr hub` — start the hub; `meshsdr hub migrate [up|down|status]` (bare `migrate` = `up`; `down` is dev only); `meshsdr hub config schema|check`; `meshsdr hub user add|remove|reset-password|list|disable|enable|exists`
- `meshsdr node` — start a node (before enrollment: TLS 1.3 with an ephemeral self-signed certificate, only `POST /enroll`, every other path 403); `meshsdr node config schema|check`; `meshsdr node enroll`
- `meshsdr all` — hub + local node (auto-enrolled over loopback)
- Global flags: `-c/--config-dir`, `--noninteractive`, `--silent`, `--json`, `--debug`

## Commands

Everything runs in Docker; no local Go toolchain required. Run `make help` for the full list.

- `make run` — all-in-one: build, generate, migrate, start the dev stack
- `make clean` — stop the stack, drop volumes (caches) and remove generated files / Air output
- `make up` / `make down` / `make logs [c=<service>]` — dev stack: Air runs `meshsdr hub` (hub.listen 8073 in the container) on http://localhost:3000 (`HTTP_PORT` to change host port), config from `.infra/docker/dev/config/` (`MESHSDR_CONFIG_DIR`)
- `make generate` — `go generate ./...` (templ, sqlc, oapi-codegen + openapi.json, tailwind)
- `make lint` / `make test`
- `make migrate [cmd=up|down|status]` — `meshsdr hub migrate` against the dev database
- `make migrate-create name=<name>` — new sequential goose SQL migration in `internal/db/sqlite/migrations/`
- `make vendor [HTMX_VERSION=x.y.z]` — refresh vendored htmx
- `make sh` — shell in dev container
- `make a11y` — axe-core WCAG 2.1 AA checks of the production image (CI-only container; Node never enters the app or dev image)
- `make build-prod` — production image (distroless, nonroot; `-f .infra/docker/Dockerfile`; config dir `/etc/meshsdr`, volume `/var/lib/meshsdr`, ports 8073/8074, `CMD ["hub"]`)

VS Code: "Reopen in Container" (`.devcontainer/`) attaches to the compose `app` service (Air keeps running). The dev image ships gopls, dlv, golangci-lint and the go.mod tools (templ, sqlc, goose, air, oapi-codegen) on `PATH`; rebuild the image after bumping tool versions.

Dev containers are rootless: the `dev` stage creates an `app` user with the host UID/GID (`UID`/`GID` build args, exported by the Makefile; the devcontainer remaps it via `updateRemoteUserUID`), so files written to the bind mount belong to the host user. Never run dev commands as root.

## Conventions

- Generated files are never committed nor edited: `*_templ.go`, `*.gen.go`, `internal/db/sqlite/sqlc/`, `internal/http/api/openapi.json`, `internal/web/static/css/app.css`, `internal/web/static/icons/`. Regenerate with `make generate`.
- Go tools are declared with the go.mod `tool` directive (`go get -tool <pkg>`) and run with `go tool <name>`.
- Schema changes go through goose migrations only, per dialect; sqlc reads the schema from `internal/db/sqlite/migrations/`. Never edit an applied migration (checksums are verified).
- Configuration only through the config structs in `internal/config` (TOML + `MESHSDR_` env; every leaf has `toml`, `env` and `jsonschema` description tags). Document new keys in `.infra/config/*.toml.example`.
- Logging only with `log/slog`.
- Migrations are not applied at startup; run `meshsdr hub migrate`. The hub refuses to start while migrations are pending, when the schema is newer than the binary, or when an applied migration's checksum differs.
- `make lint` and `make test` must pass before committing.
- Tests: stdlib `testing`, table-driven; repositories tested against real SQLite in `t.TempDir()` (`dbtest.NewSQLite`) through a shared contract suite (future adapters run the same suite; `dbtest.RunAdapterContract` covers the engine contract).
- Dependencies: stdlib and `golang.org/x/*` are fine; any other third-party dependency requires the owner's approval.
- REST: every `/api/v1` endpoint is declared in `internal/http/api/openapi.yaml` first, then generated (oapi-codegen strict chi server); module handler structs are embedded in `api.Server`. One JSON error format: RFC 9457 `application/problem+json` with a stable `code` (`internal/http/problem`).
- Git: one branch + PR per epic (`epic/<area>-<n>`), split into ordered parts when another epic needs a subset first; PR body lists `Closes #<n>` per ticket; spikes get `spike/<key>-<topic>` branches. No AI attribution in commits or PRs.
- Dockerfile (`.infra/docker/Dockerfile`, built from the repository root) stages: `base` → `dev` (Air) / `build` → `prod` (`gcr.io/distroless/static-debian13:nonroot`).
- `.infra/docker/Dockerfile.dockerignore` whitelists: ignore everything, then `!` what the build needs.

## UI

ADR 0003 and ADR 0007 are binding. In short:

- Pages render through `render.Renderer` (`Page` with an optional fragment, `Error`); handlers never write the layout themselves. A page URL returns its fragment for non-boosted htmx requests, the full page otherwise. Actions live on page-scoped paths; JSON only under `/api/v1`.
- CSP is nonce-only. In templates: no inline `<script>` (except `templ.JSONScript`), no `style=""`/`<style>`, no `hx-on`/`js:`, no templ `css`/`script` components (`web.TestTemplateRules`). Behaviour lives in ES modules under `static/js/` and custom elements (islands); untrusted text goes through `textContent`.
- Colors, type, spacing come from `--msdr-*` tokens (`static/css/input.css`, Tailwind utilities `bg-surface`, `text-fg-muted`…). New color tokens get both light and dark values and a contrast pair in `web.TestTokenContrast`. Long-lived resources (audio, WebSockets) live outside `#main`.
- Accessibility (UI-009, WCAG 2.1 AA) is part of every page's definition of done: one `h1`, labelled controls, visible focus, color never the only cue, text equivalents for canvases, throttled `aria-live` for status, usable at 200 % zoom and 320 px width. Add the page to `.infra/a11y/urls.txt`; `make a11y` (CI job `a11y`) must pass.
