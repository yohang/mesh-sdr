package jobs

import (
	"context"
	"fmt"
	"regexp"

	"github.com/yohang/mesh-sdr/internal/db"
)

var tableName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// TableStats counts the rows of a table and measures its size on disk
// (pages of the table and its indexes, from the dbstat virtual table).
type TableStats struct {
	db    *db.DB
	table string
}

// NewTableStats returns the statistics of table, a constant table name.
func NewTableStats(a *db.DB, table string) (*TableStats, error) {
	if !tableName.MatchString(table) {
		return nil, fmt.Errorf("invalid table name %q", table)
	}

	return &TableStats{db: a, table: table}, nil
}

// Stats returns the row count and the size in bytes. sized is false when
// the engine cannot tell the size.
func (s *TableStats) Stats(ctx context.Context) (rows, bytes int64, sized bool, err error) {
	q := s.db.Reader(ctx)

	// The table name is a validated constant, never user input.
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM "+s.table).Scan(&rows); err != nil { //nolint:gosec // validated constant
		return 0, 0, false, fmt.Errorf("count %s: %w", s.table, err)
	}

	err = q.QueryRowContext(ctx,
		"SELECT coalesce(sum(pgsize), 0) FROM dbstat WHERE name = ?1 OR name IN (SELECT name FROM sqlite_schema WHERE type = 'index' AND tbl_name = ?1)",
		s.table).Scan(&bytes)
	if err != nil {
		return rows, 0, false, nil //nolint:nilerr // dbstat is optional
	}

	return rows, bytes, true, nil
}
