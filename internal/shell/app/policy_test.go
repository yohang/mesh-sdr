package app_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/shell/app"
	"github.com/yohang/mesh-sdr/internal/shell/domain"
)

type policySettings struct {
	text domain.PolicyText
	set  bool
	err  error
}

func (s policySettings) UsagePolicy(context.Context) (domain.PolicyText, bool, error) {
	return s.text, s.set, s.err
}

func TestPolicy(t *testing.T) {
	var logs bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&logs, nil))
	custom := domain.MustPolicyText("Be nice.")

	tests := []struct {
		name     string
		settings policySettings
		want     domain.PolicyText
		warn     bool
	}{
		{"set", policySettings{text: custom, set: true}, custom, false},
		{"unset", policySettings{}, app.DefaultPolicy(), false},
		{"source error", policySettings{err: errors.New("db down")}, app.DefaultPolicy(), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs.Reset()

			if got := app.NewPolicy(tt.settings, logger).Text(context.Background()); got != tt.want {
				t.Errorf("text = %q", got.Markdown())
			}

			if warned := strings.Contains(logs.String(), "level=WARN"); warned != tt.warn {
				t.Errorf("warn = %v: %s", warned, logs.String())
			}
		})
	}

	if !strings.Contains(app.DefaultPolicy().Markdown(), "## Acceptable use") {
		t.Error("default policy not embedded")
	}
}
