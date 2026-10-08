package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
)

func sampleCaps() ctl.Capabilities {
	return ctl.Capabilities{
		ProductVersion: "1.0.0", Protocols: []string{"rx-ctl.v1"},
		Platform:    ctl.Platform{OS: "linux", Arch: "arm64", CPUCores: 4, Hostname: "attic"},
		SDRDrivers:  []ctl.SDRDriver{{Type: "rtl_sdr", Available: true, Version: "0.6"}, {Type: "soapy:sdrplay", Available: false, Reason: "no module"}},
		Decoders:    []ctl.Decoder{{Cap: "cap:wsjt", Tools: []ctl.Tool{{Name: "jt9", Version: "2.6", OK: true}}, Modes: []string{"ft8", "ft4"}}},
		AudioCodecs: []string{"opus"}, FFTCodecs: []string{"u8-db"},
		Codecserver: ctl.Codecserver{Available: true, AMBE: false},
	}
}

func TestCapabilityRows(t *testing.T) {
	rows, err := app.Rows(sampleCaps())
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]domain.Capability{}
	for _, r := range rows {
		got[r.Key()] = r
	}

	for key, available := range map[string]bool{
		"driver:rtl_sdr": true, "driver:soapy:sdrplay": false, "tool:jt9": true, "feature:cap:wsjt": true,
		"mode:ft8": true, "mode:ft4": true, "codec:opus": true, "codec:u8-db": true, "codec:ambe": false,
	} {
		r, ok := got[key]
		if !ok || r.Available() != available {
			t.Errorf("%s = %+v (present %v), want available %v", key, r, ok, available)
		}
	}

	if got["driver:soapy:sdrplay"].Status() != domain.CapabilityMissing || got["driver:soapy:sdrplay"].Error() != "no module" {
		t.Errorf("missing driver = %+v", got["driver:soapy:sdrplay"])
	}

	// DIAG-004: a missing decoder tool says why, on the tool, the
	// capability and its modes.
	missing := sampleCaps()
	missing.Decoders = []ctl.Decoder{{Cap: "cap:multimon-ng", Tools: []ctl.Tool{{Name: "multimon-ng", Reason: "multimon-ng not found"}}, Modes: []string{"selcall"}}}

	rows, err = app.Rows(missing)
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range rows {
		if strings.HasPrefix(r.Key(), "tool:") || strings.HasPrefix(r.Key(), "feature:") || strings.HasPrefix(r.Key(), "mode:") {
			if r.Available() || r.Error() != "multimon-ng not found" {
				t.Errorf("%s = %+v", r.Key(), r)
			}
		}
	}

	// The hash ignores seq.
	a, b := sampleCaps(), sampleCaps()
	b.Seq = 42

	ha, _, _ := app.Hash(a)
	hb, _, _ := app.Hash(b)

	if ha != hb {
		t.Error("hash depends on seq")
	}
}

func TestCapabilitiesHandler(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := enrolledNode(t, e)
	c, _ := newControl(e, "1.0.0")
	caps := app.NewCapabilities(sqlite.NewCapabilityRepository(e.db), e.nodes, nil, discard)

	reports := 0

	caps.OnReport(func(context.Context, *domain.Node, ctl.Capabilities, time.Time) error {
		reports++

		return nil
	})
	c.Handle(rxv1.TypeNodeCapabilities, caps.Handler())

	boot := welcome(t, c, n.ID(), "1.0.0")

	for seq := int64(1); seq <= 2; seq++ {
		doc := sampleCaps()
		doc.Seq = seq
		payload, _ := json.Marshal(doc)

		if _, err := c.Apply(ctx, n.ID(), boot, false, []app.Event{{Seq: seq, Type: rxv1.TypeNodeCapabilities, Payload: payload}}); err != nil {
			t.Fatal(err)
		}
	}

	if reports != 1 {
		t.Errorf("an unchanged report was applied again (%d reports)", reports)
	}

	rep, err := caps.Get(ctx, "attic")
	if err != nil || len(rep.Capabilities()) != 9 {
		t.Fatalf("report = %+v, %v", rep, err)
	}

	if got, _ := e.nodes.Get(ctx, n.ID()); got.Runtime().Hostname != "attic" || got.Runtime().CPUCores != 4 {
		t.Errorf("platform not recorded: %+v", got.Runtime())
	}

	if err := caps.Probe(ctx, "attic"); err == nil {
		t.Error("probe without a channel succeeded")
	}
}
