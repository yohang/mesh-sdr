# ADR 0006: Database adapter layout and migrations

- Status: Accepted
- Date: 2026-10-06
- Amended by: ADR 0022 (no adapter contract, `internal/db` is the SQLite package, plain goose migrations)

## Context

TECHNICAL_SPEC §3.3 and §7.2 require that the hub reach the database only through repository interfaces, implemented by one dialect adapter per engine (SQLite only in v1). The same contract suite must run against every adapter. §7.5 requires migrations that are per dialect, forward-only and checksummed.

ADR 0001 (SPK-02) chose `modernc.org/sqlite`, goose v3 (embedded SQL, applied explicitly) and sqlc. Issues #9 and #27 decide that migrations are never applied at hub start: the hub refuses to start while migrations are pending, and `meshsdr hub migrate` applies them.

## Decision

### Layout

```
internal/db/                     engine contract: Adapter, Migrator, Querier, DSN parsing, errors
internal/db/sqlite/              SQLite adapter (Open, single writer + read pool, Migrator)
internal/db/sqlite/migrations/   goose SQL migrations of the dialect (embedded)
internal/db/sqlite/queries/      sqlc queries of the dialect
internal/db/sqlite/sqlc/         sqlc output (generated, not committed)
internal/db/sqlite/sqlc.yaml     sqlc config of the dialect
internal/db/dbtest/              contract-test harness
internal/<module>/infra/sqlite/  repositories of a module for SQLite (future), using internal/db/sqlite/sqlc
internal/<module>/infra/repotest/ repository contract suites (future), run by every adapter
```

A future PostgreSQL adapter adds `internal/db/postgres/{migrations,queries,sqlc}` and `internal/<module>/infra/postgres/`.

### Engine contract (`db.Adapter`)

- `Dialect()`, `Ping`, `Close` and `Migrator()`.
- `Reader(ctx)` and `Writer(ctx)` return a `db.Querier`, which matches sqlc's `DBTX`.
- `WithinTx(ctx, fn)` runs a unit of work in one write transaction. The transaction travels in the context, so repositories join it transparently through `Reader(ctx)`/`Writer(ctx)`, and nested calls join the outer transaction. Application code declares its own transactor interface on the consumer side.
- The composition root (`wire.OpenDB`) picks the adapter from the `db.dsn` scheme:
  - `sqlite:` is the only supported scheme.
  - `postgres:` and any other scheme fail with `db_engine_unsupported`.
- `wire.Hub` takes a `db.Adapter`, not a `*sql.DB`.

### SQLite adapter

- One writer `*sql.DB` with `MaxOpenConns(1)` and `_txlock=immediate` (`BEGIN IMMEDIATE`) serialises every write.
- A separate pool of `db.max_read_connections` connections runs with `query_only(ON)`.
- Pragmas are set for every connection and checked at open: WAL, `foreign_keys=ON`, `busy_timeout=5000`, `synchronous=NORMAL`, and `auto_vacuum=INCREMENTAL` on new databases.
- The database file is created with mode 0600 when absent.
- `Ping` uses a sqlc-generated query, which keeps the sqlc pipeline exercised.

### Migrations

- goose v3 with sequential versions (`00001_name.sql`), embedded and applied by `meshsdr hub migrate [up|down|status]`. Bare `meshsdr hub migrate` means `up`.
- Each migration runs in its own transaction, which is `BEGIN IMMEDIATE` through the writer connection.
- Checksums:
  - goose has none, so the migrator keeps a `schema_migration_checksums` table (version, name, SHA-256, recorded_at).
  - `Up` refuses to run when an applied migration was modified. It then upserts the checksums of the migrations it applies (replacing a stale value left by down → edit → up) and fills in missing checksums of other applied migrations, which covers migrations applied by older binaries.
  - `Down` deletes the checksum of the migration it rolls back, on the single writer connection right after goose's rollback transaction. The two cannot share a transaction; a checksum left behind by a crash in between is replaced by the next `Up`.
- `Up` and `Down` hold an exclusive `flock` on `<db>.migrate.lock` (goose has no SQLite locker), so concurrent `meshsdr hub migrate` runs wait for each other.
- The SQLite DSN escapes `%`, `?` and `#` in the file path, because SQLite decodes URI filenames.
- `Migrator.Status` and `Migrator.Check` are read-only: they use the read pool and never create goose's table. `Check` returns:
  - `ErrSchemaTooNew` (a `VersionError` naming both versions) when the database is newer than the binary, or has an applied migration the binary does not know;
  - `ErrMigrationsPending` (a `VersionError`) when migrations are pending;
  - `ErrChecksumMismatch` when a checksum is modified or missing.
- `meshsdr hub` calls `Check` before serving and refuses to start on any of these errors. `hub migrate status` prints it (`"schema":{"ok","code","message"}` with `--json`) and exits 1 when the schema is not current.
- `down` exists for development only; production schema changes are forward-only.

### Contract tests

- `dbtest.RunAdapterContract(t, factory)` checks the engine contract on a fresh database:
  - ping;
  - the migration lifecycle (pending → up → clean check → idempotent up → status);
  - commit and rollback;
  - nested transactions;
  - read-your-writes inside a transaction;
  - the reader rejecting writes;
  - concurrent writers serialised without `SQLITE_BUSY`.
- The probe table uses portable SQL only.
- `dbtest.Adapters()` lists one factory per dialect. `ForEachAdapter`, `Migrated` and `NewSQLite` give repository suites a migrated database in `t.TempDir()`.
- SQLite-specific tests (pragmas, file mode, too-new schema, checksum mismatch, `Down`) live in `internal/db/sqlite`.

## Divergences from the spec and open points

The spec is not edited; these are recorded here instead of in issues.

1. **No migration at start.** §3.3 and §7.5.3 say the hub applies pending migrations at startup, with `hub.auto_migrate = false` to refuse instead. Per #9 and #27 the hub always refuses while migrations are pending, and `hub.auto_migrate` does not exist.
2. **Command names.** The spec's `<product> db migrate [--dry-run]`, `db status` and `admin migrate` are `meshsdr hub migrate [up|down|status]` here. `--dry-run` is not implemented.
3. **Migration naming.** §7.5.1 names migrations `<yyyymmddhhmm>_<name>`; goose sequential versions are used. The spec's forward-only rule holds in production; `down` exists for development.
4. **Version table.** §7.5.2 wants one `schema_migrations` table with a dialect column. goose's `goose_db_version` plus `schema_migration_checksums` are used. The dialect is implicit, because one database has one engine.
5. **Not implemented yet:**
   - `db.backup_before_migrate` (§7.5.8);
   - `db.sqlite.synchronous`, `db.size_warning` and `integrity_check` after an unclean shutdown;
   - the cross-dialect schema-equivalence test (§7.5.1), which needs a second adapter;
   - per-migration fixture tests (§7.5.7), to be written with the first real migrations;
   - the generic type mapping helpers, upsert, keyset pagination, batched delete and online backup parts of the §7.2 engine contract. These arrive with the repositories that need them.
6. **Blob storage is deferred to the FIL epic.** GRID-018's summary also mentions blobs with size caps (`file_blobs`); that belongs to the files feature.
7. **WAL file permissions.** `hub.db` is created with 0600; the `-wal` and `-shm` files are created by SQLite with the process umask. The service user's umask (or the 0700 `/var/lib/meshsdr` directory) must keep them private.
