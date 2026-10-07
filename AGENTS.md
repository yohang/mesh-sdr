# AGENTS.md

## Project

MeshSDR: Go web application for Software Defined Radio (SDR) with Mesh capabilities: one hub (web UI, API, DB, gateway) and N nodes (SDR devices + DSP), shipped as one binary.

- Product spec: `docs/spec/FEATURE_SPEC.md`, `docs/spec/TECHNICAL_SPEC.md` (stack-agnostic, never edited to fit the code; report inconsistencies as issues).
- Backlog: GitHub issues/milestones/epics on `yohang/mesh-sdr` (Project "MeshSDR"). Ticket keys `[GRID-001]`.
- Decisions: `docs/adr/` (spikes write ADRs as `Proposed`; the owner accepts them).
- Do not invent features or make architecture/design choices; ask.

## Stack

- Go 1.26, module `github.com/yohang/mesh-sdr`
- HTTP: `net/http` + `github.com/go-chi/chi/v5`; the hub gateway is `net/http` + `httputil.ReverseProxy` + `golang.org/x/crypto/acme/autocert` (ADR 0021, no Caddy)
- Templates: `github.com/a-h/templ`
- Frontend: htmx 4 (vendored in `internal/web/static/vendor/`), Tailwind CSS v4 (standalone CLI, no Node)
- Database: SQLite via `modernc.org/sqlite` (pure Go)
- Build: `CGO_ENABLED=1` for every binary (ADR 0014, ADR 0019); cgo only in `internal/dsp/csdr` (libcsdr++ C ABI shim, checked by a test)
- DSP: libcsdr++ (luarvique/csdr 0.18.41, built from source) through cgo, `gonum.org/v1/gonum/dsp/fourier` for the channelizer; SDR connectors are owrx_connector 0.6.5 processes
- Queries: `sqlc`; migrations: plain `goose` (embedded, applied by `meshsdr hub migrate`)
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
internal/config/        TOML + env config loading, origin tracking (file name, env var or default), JSON Schema
internal/log/           slog logger factory
internal/db/            SQLite database (`*db.DB`: single writer + read pool, WithinTx, goose migrator, DSN), see docs/adr/0006 and 0022
internal/db/sqlite/     SQLite schema: sqlc.yaml, go:generate for sqlc
internal/db/sqlite/migrations/ goose SQL migrations (embedded)
internal/db/sqlite/queries/    sqlc queries
internal/db/sqlite/sqlc/       sqlc output (generated)
internal/db/dbtest/     test helper (`dbtest.NewSQLite`: migrated database in t.TempDir())
internal/http/          chi router, middlewares, server
internal/http/api/      openapi.yaml (source of truth), oapi-codegen config, generated server, /api/v1 handlers (JSON for scripts, islands and nodes only)
internal/http/problem/  RFC 9457 problem+json errors, domain error → HTTP status
internal/web/           embedded static assets (tokens CSS, ES modules, vendored htmx), go:generate for templ + icons + tailwind
internal/web/layout/    app shell templates (document, #main, error page), shared by every module
internal/web/render/    render helper: full page vs htmx fragment, HTML headers, shell error pages
internal/web/icongen/   icon generator (go:generate, golang.org/x/image/vector)
internal/dsp/           node DSP: rings with gap markers, shared spectrum, FFT channelizer, NFM chain, audio framing (no I/O, no goroutines)
internal/dsp/csdr/      cgo shim over libcsdr++ (the only cgo package)
internal/protocol/rxv1/ rx.v1 codec, payloads (ctl, media), tokens, wsconn adapter, sendq (§6.8 media send queue)
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

- `grid`: nodes, enrollment, internal CA / mTLS, control channel, heartbeat, capabilities, device registry, gateway (`infra/gateway`, net/http: TLS, hub router, node media proxy) and its forward auth, node media WebSocket and access-token verification
- `identity`: users, roles, sessions, passwords, invitations, access tokens, CSRF, audit log
- `settings`: DB settings store, config locking/precedence, retention
- `shell`: app shell UI (layout, navigation, theming, static pages)
- `radio`: node devices (ADR 0019): device lifecycle and manager, owrx connectors under the process supervisor (`infra/process`, ADR 0017), DSP engine (`infra/engine`), media stream handler (`http`); wired by `internal/radio/wire.go`

## Architecture (light DDD)

Code is organized by module, layered inside each module:

```
internal/<module>/
  domain/   aggregates, entities, value objects, domain errors, repository interfaces
  app/      use cases / application services (orchestrate domain + ports)
  infra/    adapters: repositories (infra/sqlite/, sqlc, taking a *db.DB), external systems, hardware
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
- `internal/wire` is the composition root: `wire.Hub(cfg, logger, adapter)` and `wire.Node(cfg, logger, now)` build the object graph of each role from its config, `*slog.Logger` and, for the hub, the `*db.DB` opened by `wire.OpenDB` (from `db.dsn`, `sqlite:` only). CLI commands call them.
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
- Every key can be overridden by an env var: `MESHSDR_` prefix, `__` between nesting levels (`db.dsn` → `MESHSDR_DB__DSN`, `gateway.tls_mode` → `MESHSDR_GATEWAY__TLS_MODE`). Env-set keys are locked like file-set keys, with origin `env:<VAR>`.
- Precedence: env > config files > DB settings > defaults. Config/env keys are read-only (locked) in the UI, which shows their origin (the file name, such as `hub.toml`, or `env:MESHSDR_…`).
- Typed Go structs are the source of truth; JSON Schema (draft 2020-12) is generated from them (`meshsdr hub|node config schema`, attached to releases by CI). Validation happens at load; invalid config is a startup error (exit code 78). `meshsdr hub|node config check` validates and prints each key's origin. See docs/adr/0005.
- The binary never writes config files (only exception: first-start TLS bootstrap of `all`).

## 12-factor

- Environment-specific values come from config files or `MESHSDR_*` env vars (see Configuration); never hard-coded.
- Logs as event streams to stderr; no log files, no rotation.
- Stateless processes; persistent state only in backing services (hub SQLite file at `db.dsn`, `/var/lib/meshsdr` volume in Docker); nodes hold no persistent state.
- Port binding: the hub's gateway owns every public port (`gateway.https_listen`, default `:443`, and `gateway.http_listen`; TLS from `gateway.tls_mode`: acme (autocert, cache in `gateway.storage_dir`), files or off; the nonroot image binds :443 thanks to Docker's default `ip_unprivileged_port_start=0`, elsewhere use `:8443` plus a port mapping), nodes listen on `node.listen` (default `0.0.0.0:8074`); app is self-contained (embedded assets/migrations/OpenAPI document). See docs/adr/0012 and docs/adr/0021.
- Disposability: fast start, graceful shutdown on SIGTERM/SIGINT.
- Admin tasks as one-off subcommands of the same binary (`meshsdr hub migrate`, `meshsdr hub user …`).
- Build/release/run separated: immutable image, config injected at runtime. Dev/prod parity through the same Dockerfile.
- Dependencies explicitly declared (go.mod, pinned tool/asset versions).

## CLI

One binary; the bare role starts the process, admin tasks are subcommands of the role:

- `meshsdr hub` — start the hub; `meshsdr hub migrate [up|down|status]` (bare `migrate` = `up`; `down` is dev only); `meshsdr hub config schema|check`; `meshsdr hub ca init` (hub internal CA in `<config-dir>/tls`, never overwrites); `meshsdr hub node add|list|show|token|disable|enable|revoke|remove`; `meshsdr hub user add|remove|reset-password|list|disable|enable|exists`. The admin subcommands (`migrate`, `node`, `user`, `keys`; not the bare `hub` nor `hub config check`, which validates what `hub` runs) default `tls.ca_cert`/`tls.ca_key` to `<config-dir>/tls/ca.pem`/`ca.key` when both files exist, like `all`
- `meshsdr node` — start a node (enrolled: mTLS node API with `/control`; before enrollment: TLS 1.3 with an ephemeral self-signed certificate, every path 403, `POST /enroll` 501); `meshsdr node config schema|check`; `meshsdr node enroll` (one-off: serves `POST /enroll` until the hub enrolls the node, writes `tls.key`, `tls.cert`, `hub_trust.ca_cert`, exits). See docs/adr/0008
- `meshsdr all` — hub + local node in one process (node id `local` on `127.0.0.1:8074` by default): creates the hub CA (`tls/ca.pem`, `tls/ca.key`) and the local node certificate (`tls/node.pem`, `tls/node.key`) on first start, enrolls the local node in-process; `meshsdr all config check` (prints each key's origin for both files). See docs/adr/0012
- Global flags: `-c/--config-dir`, `--noninteractive`, `--silent`, `--json`, `--debug`

## Commands

Everything runs in Docker; no local Go toolchain required. Run `make help` for the full list.

- `make run` — all-in-one: build, generate, migrate, start the dev stack
- `make clean` — stop the stack, drop volumes (caches) and remove generated files / Air output
- `make up` / `make down` / `make logs [c=<service>]` — dev stack: Air runs `meshsdr all` (gateway with `tls_mode = off` on 8073 in the container, local node `dev`) on http://localhost:3000 (`HTTP_PORT` to change host port), config from `.infra/docker/dev/config/` (`MESHSDR_CONFIG_DIR`)
- `make generate` — `go generate ./...` (templ, sqlc, oapi-codegen + openapi.json, tailwind)
- `make lint` / `make test`
- `make migrate [cmd=up|down|status]` — `meshsdr hub migrate` against the dev database
- `make migrate-create name=<name>` — new sequential goose SQL migration in `internal/db/sqlite/migrations/`
- `make vendor [HTMX_VERSION=x.y.z]` — refresh vendored htmx
- `make sh` — shell in dev container
- `make a11y` — axe-core WCAG 2.1 AA checks of the production image, one hub with theme mode auto (CI-only container, nightly CI job; Node never enters the app or dev image)
- `make build-prod` — the production image (Debian slim with the connectors, nonroot 65532, run with `init: true`; `-f .infra/docker/Dockerfile`; config dir `/etc/meshsdr` with `tls/` linked to the volume `/var/lib/meshsdr`): target `prod` (`meshsdr`, one image for every role: port 443 and `CMD ["all"]` by default, `node` as command for a node, port 8074); CI publishes it on pushes to `main` and tags

VS Code: "Reopen in Container" (`.devcontainer/`) attaches to the compose `app` service (Air keeps running). The dev image ships gopls, dlv, golangci-lint and the go.mod tools (templ, sqlc, goose, air, oapi-codegen) on `PATH`; rebuild the image after bumping tool versions.

Dev containers are rootless: the `dev` stage creates an `app` user with the host UID/GID (`UID`/`GID` build args, exported by the Makefile; the devcontainer remaps it via `updateRemoteUserUID`), so files written to the bind mount belong to the host user. Never run dev commands as root.

## Conventions

- Generated files are never committed nor edited: `*_templ.go`, `*.gen.go`, `internal/db/sqlite/sqlc/`, `internal/http/api/openapi.json`, `internal/web/static/css/app.css`, `internal/web/static/icons/`. Regenerate with `make generate`.
- Go tools are declared with the go.mod `tool` directive (`go get -tool <pkg>`) and run with `go tool <name>`.
- Schema changes go through goose migrations only; sqlc reads the schema from `internal/db/sqlite/migrations/`. Never edit an applied migration (nothing detects it).
- Configuration only through the config structs in `internal/config` (TOML + `MESHSDR_` env; every leaf has `toml`, `env` and `jsonschema` description tags). Document new keys in `.infra/config/*.toml.example`.
- Logging only with `log/slog`.
- Migrations are not applied at startup; run `meshsdr hub migrate` (one run at a time, no lock). The hub refuses to start while migrations are pending or when the schema is newer than the binary.
- `make lint` and `make test` must pass before committing.
- Tests: stdlib `testing`, table-driven; repositories tested against real SQLite in `t.TempDir()` (`dbtest.NewSQLite`).
- Dependencies: stdlib and `golang.org/x/*` are fine; any other third-party dependency requires the owner's approval.
- REST: `/api/v1` only for JSON needed by JS/islands/nodes (and the resources pages link to); UI actions are HTML forms; no API twins (ADR 0023). Every `/api/v1` endpoint is declared in `internal/http/api/openapi.yaml` first, then generated (oapi-codegen strict chi server); module handler structs are embedded in `api.Server`. One JSON error format: RFC 9457 `application/problem+json` with a stable `code` (`internal/http/problem`).
- Git: one branch + PR per epic (`epic/<area>-<n>`), split into ordered parts when another epic needs a subset first; PR body lists `Closes #<n>` per ticket; spikes get `spike/<key>-<topic>` branches. No AI attribution in commits or PRs.
- Dockerfile (`.infra/docker/Dockerfile`, built from the repository root) stages: `natives` (csdr + owrx_connector from pinned tarballs) → `base` (Go + C/C++ toolchain, cgo) → `dev` (Air) / `build` → `runtime` (`debian:trixie-slim` by digest, pinned apt packages, nonroot 65532, licence notices) → `prod`; `sources` (GPL sources, `-sources` tag). Rebuild the dev image after changing a native or apt pin.
- `.infra/docker/Dockerfile.dockerignore` whitelists: ignore everything, then `!` what the build needs.

## UI

ADR 0003 and ADR 0007 are binding. In short:

- Pages render through `render.Renderer` (`Page` with an optional fragment, `Error`); handlers never write the layout themselves. A page URL returns its fragment for non-boosted htmx requests, the full page otherwise. Actions live on page-scoped paths; JSON only under `/api/v1`.
- CSP is nonce-only. In templates: no inline `<script>` (except `templ.JSONScript`), no `style=""`/`<style>`, no `hx-on`/`js:`, no templ `css`/`script` components (`web.TestTemplateRules`). Behaviour lives in ES modules under `static/js/` and custom elements (islands); untrusted text goes through `textContent`.
- Colors, type, spacing come from `--msdr-*` tokens (`static/css/input.css`, Tailwind utilities `bg-surface`, `text-fg-muted`…). New color tokens get both light and dark values and a contrast pair in `web.TestTokenContrast`. Long-lived resources (audio, WebSockets) live outside `#main`.
- Accessibility (UI-009, WCAG 2.1 AA) is part of every page's definition of done: one `h1`, labelled controls, visible focus, color never the only cue, text equivalents for canvases, throttled `aria-live` for status, usable at 200 % zoom and 320 px width. Add the page to `.infra/a11y/urls.txt`; `make a11y` (CI job `a11y`) must pass.
