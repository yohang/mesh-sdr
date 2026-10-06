package db

import (
	"errors"
	"fmt"
	"strings"
)

// Dialect is a database engine supported by an adapter.
type Dialect string

// Dialects.
const (
	DialectSQLite Dialect = "sqlite"
)

var (
	// ErrInvalidDSN is returned for a malformed db.dsn.
	ErrInvalidDSN = errors.New("invalid DSN")
	// ErrEngineUnsupported is returned for a DSN scheme without adapter
	// (TECHNICAL_SPEC §7.2 "Engine selection": db_engine_unsupported).
	ErrEngineUnsupported = errors.New("db_engine_unsupported")
)

// DSN is a parsed db.dsn: the scheme selects the adapter.
type DSN struct {
	dialect Dialect
	path    string
}

// ParseDSN parses a db.dsn. Supported forms: sqlite:///abs/path, sqlite:/abs/path
// and sqlite:relative/path. postgres: and postgresql: are reserved for a
// future adapter and rejected with ErrEngineUnsupported.
func ParseDSN(s string) (DSN, error) {
	scheme, rest, ok := strings.Cut(s, ":")
	if !ok || scheme == "" {
		return DSN{}, fmt.Errorf("%w %q: missing scheme (want sqlite:…)", ErrInvalidDSN, s)
	}

	switch strings.ToLower(scheme) {
	case "sqlite":
		path := rest
		if strings.HasPrefix(path, "//") {
			// sqlite://<host>/<path>: only an empty host is allowed.
			host, p, _ := strings.Cut(path[2:], "/")
			if host != "" {
				return DSN{}, fmt.Errorf("%w %q: host not supported, use sqlite:///abs/path", ErrInvalidDSN, s)
			}

			path = "/" + p
		}

		if path == "" || path == "/" || strings.ContainsAny(path, "?#") {
			return DSN{}, fmt.Errorf("%w %q: want sqlite:///abs/path or sqlite:relative/path (no query string)", ErrInvalidDSN, s)
		}

		return DSN{dialect: DialectSQLite, path: path}, nil
	case "postgres", "postgresql":
		return DSN{}, fmt.Errorf("%w: %s (only sqlite is supported)", ErrEngineUnsupported, scheme)
	default:
		return DSN{}, fmt.Errorf("%w: %s (only sqlite is supported)", ErrEngineUnsupported, scheme)
	}
}

// Dialect returns the engine selected by the scheme.
func (d DSN) Dialect() Dialect { return d.dialect }

// Path returns the database file path (SQLite).
func (d DSN) Path() string { return d.path }
