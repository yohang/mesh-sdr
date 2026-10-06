// Package migrations embeds the goose SQL migrations.
package migrations

import "embed"

// FS contains the SQL migration files.
//
//go:embed *.sql
var FS embed.FS
