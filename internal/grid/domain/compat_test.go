package domain_test

import (
	"testing"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
)

func TestCheckCompatibility(t *testing.T) {
	ctlv1 := []string{"rx-ctl.v1", "rx.v1"}

	tests := []struct {
		hub, node string
		protocols []string
		level     domain.CompatLevel
		hint      string
	}{
		{"1.4.2", "1.4.0", ctlv1, domain.CompatOK, ""},
		{"1.4.2", "1.5.1", ctlv1, domain.CompatOK, ""},
		{"1.4.2", "1.3.9", ctlv1, domain.CompatOlder, domain.HintUpgradeRecommended},
		{"1.4.2", "1.2.0", ctlv1, domain.CompatIncompatible, domain.HintNodeTooOld},
		{"1.4.2", "2.0.0", ctlv1, domain.CompatIncompatible, domain.HintNodeIncompatible},
		{"2.0.0", "1.9.0", ctlv1, domain.CompatIncompatible, domain.HintNodeIncompatible},
		{"0.4.1", "0.4.1", ctlv1, domain.CompatOK, ""},
		{"0.4.1", "0.4.2", ctlv1, domain.CompatOK, ""},
		{"0.4.3", "0.4.2", ctlv1, domain.CompatOlder, domain.HintUpgradeRecommended},
		{"0.4.3", "0.3.9", ctlv1, domain.CompatIncompatible, domain.HintNodeIncompatible},
		{"v1.4.2", "v1.4.2-rc.1", ctlv1, domain.CompatOK, ""},
		{"dev", "1.0.0", ctlv1, domain.CompatOK, domain.HintDevBuild},
		{"1.0.0", "(devel)", ctlv1, domain.CompatOK, domain.HintDevBuild},
		{"1.0.0", "0.0.0-20261006120000-abcdef123456", ctlv1, domain.CompatOK, domain.HintDevBuild},
		{"1.0.0", "garbage", ctlv1, domain.CompatIncompatible, domain.HintNodeIncompatible},
		{"1.0.0", "1.0.0", []string{"rx.v1"}, domain.CompatIncompatible, domain.HintProtocol},
	}

	for _, tt := range tests {
		got := domain.CheckCompatibility(tt.hub, tt.node, tt.protocols)
		if got.Level != tt.level || got.Hint != tt.hint {
			t.Errorf("hub %s node %s: %+v, want %s %q", tt.hub, tt.node, got, tt.level, tt.hint)
		}
	}
}

func TestParseSemVer(t *testing.T) {
	for _, in := range []string{"1.2", "1.2.3.4", "01.2.3", "a.b.c", "1.-2.3"} {
		if _, err := domain.ParseSemVer(in); err == nil {
			t.Errorf("ParseSemVer(%q) accepted", in)
		}
	}
}
