package db

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrInvalidDSN is returned for a malformed db.dsn.
	ErrInvalidDSN = errors.New("invalid DSN")
	// ErrEngineUnsupported is returned for a DSN scheme other than sqlite
	// (TECHNICAL_SPEC §7.2 "Engine selection": db_engine_unsupported).
	ErrEngineUnsupported = errors.New("db_engine_unsupported")
)

// ParseDSN parses a db.dsn and returns the database file path. Supported
// forms: sqlite:///abs/path, sqlite:/abs/path and sqlite:relative/path. Any
// other scheme fails with ErrEngineUnsupported.
func ParseDSN(s string) (string, error) {
	scheme, path, ok := strings.Cut(s, ":")
	if !ok || scheme == "" {
		return "", fmt.Errorf("%w %q: missing scheme (want sqlite:…)", ErrInvalidDSN, s)
	}

	if !strings.EqualFold(scheme, "sqlite") {
		return "", fmt.Errorf("%w: %s (only sqlite is supported)", ErrEngineUnsupported, scheme)
	}

	if strings.HasPrefix(path, "//") {
		// sqlite://<host>/<path>: only an empty host is allowed.
		host, p, _ := strings.Cut(path[2:], "/")
		if host != "" {
			return "", fmt.Errorf("%w %q: host not supported, use sqlite:///abs/path", ErrInvalidDSN, s)
		}

		path = "/" + p
	}

	if path == "" || path == "/" || strings.ContainsAny(path, "?#") {
		return "", fmt.Errorf("%w %q: want sqlite:///abs/path or sqlite:relative/path (no query string)", ErrInvalidDSN, s)
	}

	return path, nil
}
