// Package log builds the application slog logger.
package log

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New returns a slog logger writing to w with the given level and format (json|text).
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", level, err)
	}

	opts := &slog.HandlerOptions{Level: lvl}

	switch strings.ToLower(format) {
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("invalid log format %q (expected json or text)", format)
	}
}
