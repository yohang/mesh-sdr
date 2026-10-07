package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/events/domain"
)

func TestParseTopic(t *testing.T) {
	valid := []string{
		"nodes", "devices", "presence", "map", "notifications", "admin.connections",
		"decodes:device=dev-hf", "diagnostics:device=a_1",
	}
	for _, s := range valid {
		tp, err := domain.ParseTopic(s)
		if err != nil {
			t.Errorf("ParseTopic(%q) = %v", s, err)

			continue
		}

		if tp.String() != s {
			t.Errorf("round trip %q → %q", s, tp.String())
		}
	}

	invalid := []string{
		"", "Nodes", "nodes:device=x", "decodes", "decodes:device=", "decodes:node=x",
		"decodes:device=a b", "admin", "admin.users", "decodes:device=" + strings.Repeat("a", 65),
	}
	for _, s := range invalid {
		if _, err := domain.ParseTopic(s); !errors.Is(err, domain.ErrInvalidTopic) {
			t.Errorf("ParseTopic(%q) = %v, want invalid_topic", s, err)
		}
	}

	if d := domain.MustTopic("decodes:device=dev-hf").Device(); d != "dev-hf" {
		t.Errorf("Device() = %q", d)
	}

	if !domain.MustTopic("admin.connections").IsAdmin() || domain.MustTopic("nodes").IsAdmin() {
		t.Error("IsAdmin")
	}
}
