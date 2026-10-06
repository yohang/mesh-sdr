// Package infra holds the shell adapters.
package infra

import (
	"context"
	"fmt"
	"strings"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/shell/domain"
)

// ConfigSettings reads the shell settings from the hub config ([settings]).
//
// It is the interim source until the DB settings store exists (ADM-002, ADR
// 0007): the effective value is then the config value (locked) or the
// built-in default. The settings store will replace it with config > DB >
// default behind the same app.Settings port.
type ConfigSettings struct {
	cfg config.Settings
}

// NewConfigSettings returns a ConfigSettings over cfg.
func NewConfigSettings(cfg config.Settings) *ConfigSettings {
	return &ConfigSettings{cfg: cfg}
}

// ThemeMode returns settings.ui.theme_mode.
func (s *ConfigSettings) ThemeMode(context.Context) (domain.ThemeMode, error) {
	m, err := domain.NewThemeMode(s.cfg.UI.ThemeMode)
	if err != nil {
		return domain.ThemeMode{}, fmt.Errorf("settings.ui.theme_mode: %w", err)
	}

	return m, nil
}

// UsagePolicy returns settings.receiver.usage_policy_text; set is false when
// it is empty.
func (s *ConfigSettings) UsagePolicy(context.Context) (domain.PolicyText, bool, error) {
	if strings.TrimSpace(s.cfg.Receiver.UsagePolicyText) == "" {
		return domain.PolicyText{}, false, nil
	}

	p, err := domain.NewPolicyText(s.cfg.Receiver.UsagePolicyText)
	if err != nil {
		return domain.PolicyText{}, false, fmt.Errorf("settings.receiver.usage_policy_text: %w", err)
	}

	return p, true, nil
}
