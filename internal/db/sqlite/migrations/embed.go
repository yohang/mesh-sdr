// Package migrations embeds the goose SQL migrations of the SQLite dialect.
//
// Versions are sequential (00001_name.sql). Migrations are forward-only in
// production; tables are declared STRICT and use the generic type mapping of
// TECHNICAL_SPEC §7.2.
package migrations

import "embed"

// FS contains the SQL migration files.
//
//go:embed *.sql
var FS embed.FS
