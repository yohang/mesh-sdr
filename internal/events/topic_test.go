package events_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/events"
)

func TestParseTopic(t *testing.T) {
	valid := []string{
		"nodes", "devices", "presence", "map", "notifications", "admin.connections",
		"decodes:device=dev-hf", "diagnostics:device=a_1", "admin.device_log:device=hf",
	}
	for _, s := range valid {
		tp, err := events.ParseTopic(s)
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
		"decodes:device=a b", "admin", "admin.users", "device_log:device=hf", "admin.device_log", "decodes:device=" + strings.Repeat("a", 65),
	}
	for _, s := range invalid {
		if _, err := events.ParseTopic(s); !errors.Is(err, events.ErrInvalidTopic) {
			t.Errorf("ParseTopic(%q) = %v, want invalid_topic", s, err)
		}
	}

	if d := events.MustTopic("decodes:device=dev-hf").Device(); d != "dev-hf" {
		t.Errorf("Device() = %q", d)
	}

	if !events.MustTopic("admin.connections").IsAdmin() || events.MustTopic("nodes").IsAdmin() {
		t.Error("IsAdmin")
	}

	if tp := events.MustTopic("admin.device_log:device=hf"); !tp.IsAdmin() || tp.Device() != "hf" {
		t.Errorf("device log topic: admin %v, device %q", tp.IsAdmin(), tp.Device())
	}
}
