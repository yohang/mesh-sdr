-- Migrations are plain goose (ADR 0022): the checksum table kept by the
-- former migrator is no longer used.

-- +goose Up
DROP TABLE IF EXISTS schema_migration_checksums;

-- +goose Down
