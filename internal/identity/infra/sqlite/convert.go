// Package sqlite implements the identity repositories for the SQLite dialect
// with the sqlc queries of internal/db/sqlite. Rows are mapped to aggregates
// here; timestamps are Unix epoch milliseconds (UTC).
package sqlite

import (
	"database/sql"
	"net/netip"
	"time"
)

func ms(t time.Time) int64 { return t.UnixMilli() }

func nullMS(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func fromNullMS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}

	return fromMS(v.Int64)
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}

	return 0
}

func nullIP(a netip.Addr) sql.NullString {
	if !a.IsValid() {
		return sql.NullString{}
	}

	return sql.NullString{String: a.Unmap().WithZone("").String(), Valid: true}
}

// parseIP ignores an unparsable stored address: it is informational.
func parseIP(s sql.NullString) netip.Addr {
	if !s.Valid {
		return netip.Addr{}
	}

	a, err := netip.ParseAddr(s.String)
	if err != nil {
		return netip.Addr{}
	}

	return a.Unmap()
}
