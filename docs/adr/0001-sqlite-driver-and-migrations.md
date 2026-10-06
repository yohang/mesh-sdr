# ADR 0001: SQLite driver, migration tooling and query generation

- Status: Proposed
- Date: 2026-10-06

## Context

The hub persists its state in SQLite (the only engine in v1) behind repository interfaces and a per-engine dialect adapter, with a PostgreSQL adapter as future work (TECHNICAL_SPEC §3.3 and §7.2). The hub is the single process that opens the database and the single writer; nodes never touch it.

The SQLite driver and the migration tooling were chosen during project bootstrap and are already in place. This ADR only records those decisions and their rationale. It does not change the product specification.

Constraints that shaped the choices:

- The binary is built with `CGO_ENABLED=0` (see the `build` stage of the `Dockerfile`) and shipped in a distroless static image (`gcr.io/distroless/static-debian13:nonroot`), which has no C runtime.
- Nodes run on ARM boards, so cross-compilation must stay simple.
- Assets and migrations are embedded in the binary (self-contained app).

## Decision

### Driver

Use `modernc.org/sqlite` (pure Go, registered as the `sqlite` `database/sql` driver in `internal/db/db.go`; version pinned in `go.mod`). It requires no cgo, so builds run with `CGO_ENABLED=0`, cross-compile to ARM with the plain Go toolchain, and run in a distroless static image.

### Connection pragmas

`internal/db/db.go` opens the database with these pragmas, passed in the DSN so they apply to every pooled connection:

| Pragma | Value | Rationale |
|---|---|---|
| `journal_mode` | `WAL` | Concurrent readers during writes. |
| `foreign_keys` | `ON` | SQLite does not enforce foreign keys by default. |
| `busy_timeout` | `5000` | Wait up to 5 s on a locked database instead of failing at once. |
| `synchronous` | `NORMAL` | Safe with WAL, avoids an fsync per transaction. |

These match the "SQLite mode" row of TECHNICAL_SPEC §3.3. The single-writer rule (one writer connection plus a read pool) belongs to the repository and dialect adapter layer and is not part of this decision.

### Migrations

- Tool: goose v3 (`github.com/pressly/goose/v3`), used as a library through `goose.NewProvider` with the SQLite dialect.
- Format: plain SQL files in `internal/db/migrations/`, embedded in the binary with `embed.FS`.
- Policy: migrations are applied explicitly by the `migrate` subcommand (`migrate up|down|status`, which will become `meshsdr hub migrate`). They are never applied at process start. The hub is to refuse to start while migrations are pending (GRID-019, issue #27).
- Direction: forward-only per the spec (TECHNICAL_SPEC §7.5). The `down` command exists in the tooling, but production schema changes are not designed around rollbacks.

### Queries

Use sqlc (`sqlc.yaml`, engine `sqlite`). The schema is read from `internal/db/migrations/`, queries are written in `internal/db/queries/`, and Go code is generated into `internal/db/sqlc/`. Generated code is not committed; it is produced by `make generate`.

### Out of scope and open

Backup settings (online backup method, schedule, backup before migrate, retention) are not decided here. TECHNICAL_SPEC §3.3 and §7.2 describe requirements for them, and they remain open for a later issue.

## Consequences

Positive:

- No C toolchain in the build, simple ARM cross-compilation, small static distroless image.
- A single binary carries its migrations; schema changes are visible, reviewable SQL.
- Explicit migration keeps schema changes an operator action and keeps start-up fast and predictable.
- sqlc gives type-checked queries without an ORM, and the schema has a single source of truth (the migrations).

Negative and risks:

- `modernc.org/sqlite` is a transpilation of the C SQLite library; it is generally slower than the cgo driver and can lag behind upstream SQLite releases. Acceptable for the expected load of a single-writer hub.
- Pragmas are set per connection through the DSN; any new way of opening the database must go through `db.Open` to keep them.
- Because migrations are not applied at start, an operator can start a new binary against an old schema. This is why the hub must refuse to start with pending migrations (issue #27).
- The explicit-migration policy differs from the wording of TECHNICAL_SPEC §3.3 and §7.5, which describe the hub applying pending migrations at startup (with `hub.auto_migrate = false` as the refuse-to-start mode) and name the commands `<product> db migrate` and `admin migrate`. This ADR records the bootstrap decision and does not edit the spec; if the spec must change, a separate issue is needed.
- sqlc's SQLite dialect is engine-specific. A future PostgreSQL adapter will need its own query set and migration set, as the spec already requires per-dialect migrations and adapters (issue #26, GRID-018).

## Alternatives considered

- `github.com/mattn/go-sqlite3` (cgo): the most widely used and fastest driver, but it requires cgo. That breaks `CGO_ENABLED=0`, complicates ARM cross-compilation (needs a C cross-compiler), and does not run in a distroless static image without extra work. Rejected for those reasons.
- Applying migrations automatically at start: simpler for the operator, but hides schema changes in the start-up path and makes failures harder to separate from service start. Rejected in favour of an explicit `migrate` command plus a refuse-to-start check.

Other migration tools and query layers were not evaluated in this spike; the choices above were made at bootstrap.

## References

- Issue #2: [SPK-02] Spike: SQLite driver and migration tooling
- Issue #26 (GRID-018) and issue #27 (GRID-019), unblocked by this ADR
- `docs/spec/TECHNICAL_SPEC.md` §3.3 Persistence, §7.2 Database adapter, §7.5 Migrations
- `internal/db/db.go`, `internal/db/migrations/`, `internal/cli/migrate.go`, `sqlc.yaml`, `Dockerfile`
