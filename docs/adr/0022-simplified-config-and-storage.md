# ADR 0022: Simplified configuration and storage

- Status: Accepted
- Date: 2026-10-07
- Deciders: project owner
- Amends: ADR 0005, ADR 0006, ADR 0010, ADR 0020

## Context

Several M0 mechanisms cost more code and tests than they return while the product has a single engine and few consumers: per-key line tracking in config origins, a "did you mean" for env vars, settings that nothing reads, a dialect-neutral database contract with one dialect, migration checksums and a migration lock, and a reporting engine without a transport. The owner asked to remove them and keep every other behaviour.

## Decision

1. **Config origins (ADR 0005).** An origin is the file name relative to the config dir (`hub.toml`, `hub.d/10-site.toml`), `env:<VAR>` or `default`. The in-house TOML position scanner (`tomlpos.go`) is removed. Keys set by a file or the env stay locked, and the admin UI shows the origin. Parse errors keep the `file:line:column` given by the TOML library. An unknown `MESHSDR_*` variable is still an error (`unknown_env_var`), without a suggestion. Unknown keys in files keep their "did you mean" hint (TECHNICAL_SPEC §7.4 step 3).
2. **Settings without a consumer (ADR 0010).** `bandplan.region`, `bookmarks.eibi_range_km`, `bookmarks.repeater_range_km`, `receiver.altitude_m`, `receiver.country`, `receiver.admin_email`, `receiver.admin_email_public`, `ui.tuning_precision`, `ui.recorder_enabled` and `ui.layout.*` are removed from `config.Settings`, the admin forms and the samples. A feature that reads one adds it back. A DB row left for a removed key is ignored at load, as for any unknown key.
3. **Database (ADR 0006).** The `db.Adapter`, `db.Migrator` and `db.Dialect` interfaces and the engine contract suite (`dbtest/contract.go`) are removed. `internal/db` is the SQLite package: `db.Open` returns a `*db.DB` (single writer, read pool, `Reader`, `Writer`, `WithinTx`, `Migrator`), which `wire.OpenDB` returns and repositories take. `db.dsn` still accepts `sqlite:` only; other schemes fail with `db_engine_unsupported`. `internal/db/sqlite/` keeps the migrations, the queries, `sqlc.yaml` and the sqlc output. `dbtest.NewSQLite` returns a migrated database in `t.TempDir()`.
4. **Migrations (ADR 0006).** Plain goose: no checksum table and no `flock`. `meshsdr hub migrate [up|down|status]` is unchanged, and the hub still refuses to start while migrations are pending or when the schema is newer than the binary. Migration `00035` drops `schema_migration_checksums`. Never edit an applied migration.
5. **Reporting (ADR 0020).** `internal/reporting`, its worker, `GET /api/v1/reporting/status`, the `outbox.purge` job, the `retention.reporting_outbox.*` settings and the outbox row of Admin › Data & retention are removed. Migration `00034` drops `reporting_outbox`. RPT-001 brings the outbox back with the first transport.
6. **Identity defaults (ADR 0010, ADR 0011).** The interim `identity/infra/settings` package is removed: the store covers its keys. CLI commands and tests, which run without the store, use `identity/app.DefaultSettings`, next to `app.DefaultSessionPolicies`.

## Consequences

- About 2 600 lines less in net (Go, SQL, YAML and tests).
- Origins no longer point at a line; a key can be found in the named file.
- A future PostgreSQL engine has to reintroduce an abstraction.
- Two concurrent `meshsdr hub migrate` runs are no longer serialised; run one at a time.
- An applied migration that is edited is no longer detected.
