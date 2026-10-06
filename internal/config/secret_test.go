package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestSecretIsRedacted(t *testing.T) {
	var s Secret
	if err := s.UnmarshalText([]byte("s3cret")); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer

	slog.New(slog.NewJSONHandler(&logged, nil)).Info("x", slog.Any("password", s))

	outputs := map[string]string{
		"%v":     fmt.Sprintf("%v", s),
		"Sprint": fmt.Sprint(s),
		"%+v":    fmt.Sprintf("%+v", s),
		"%#v":    fmt.Sprintf("%#v", s),
		"slog":   logged.String(),
	}

	for format, out := range outputs {
		if strings.Contains(out, "s3cret") || !strings.Contains(out, "[redacted]") {
			t.Errorf("%s leaks or misses the placeholder: %s", format, out)
		}
	}

	if s.Reveal() != "s3cret" {
		t.Error("Reveal() lost the value")
	}
}
