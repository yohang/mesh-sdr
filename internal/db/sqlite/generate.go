// Package sqlite holds the SQLite schema (goose migrations), the queries
// and the sqlc configuration; sqlc writes the query code to sqlite/sqlc.
// The database handle is package internal/db.
package sqlite

//go:generate go tool sqlc generate --file sqlc.yaml
