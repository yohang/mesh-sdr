package sqlite_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// softwareVersionMigration rebuilds nodes for 64-character versions.
const softwareVersionMigration = 41

func TestSoftwareVersionLength(t *testing.T) {
	ctx := context.Background()
	a := dbtest.NewSQLite(t)
	nodes := sqlite.NewNodeRepository(a)
	id := domain.MustNodeID("attic")

	if err := nodes.Create(ctx, domain.NewNode(id, domain.MustNodeName("a"), domain.MustNodeURL("https://x:1"), t0)); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		version string
		wantErr bool
	}{
		{"dev pseudo-version", "0.0.0-20261008191410-309bcfdfb3d7+dirty", false},
		{"64 characters", strings.Repeat("v", 64), false},
		{"65 characters", strings.Repeat("v", 65), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := a.Writer(ctx).ExecContext(ctx, "UPDATE nodes SET software_version = ? WHERE id = ?", tt.version, id.String())
			if tt.wantErr {
				if !db.IsConstraint(err) {
					t.Fatalf("update = %v, want a constraint error", err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if got := must(nodes.Get(ctx, id)).Runtime().SoftwareVersion; got != tt.version {
				t.Errorf("software_version = %q, want %q", got, tt.version)
			}
		})
	}

	// The welcome cuts a longer version to the column limit.
	n := must(nodes.Get(ctx, id))
	n.RecordWelcome(must(shared.NewUUIDv7(t0)), strings.Repeat("w", 80), "rx-ctl.v1")

	if err := nodes.SaveRuntime(ctx, n); err != nil {
		t.Fatal(err)
	}

	if got := must(nodes.Get(ctx, id)).Runtime().SoftwareVersion; got != strings.Repeat("w", 64) {
		t.Errorf("welcome software_version = %q", got)
	}

	// The capability report's product version has the same limit.
	long := strings.Repeat("p", 64)
	rep := must(domain.NewCapabilityReport(id, "h", long, []string{"rx-ctl.v1"}, json.RawMessage(`{}`), json.RawMessage(`{}`), t0, nil))

	caps := sqlite.NewCapabilityRepository(a)
	if err := caps.Replace(ctx, rep); err != nil {
		t.Fatal(err)
	}

	if got := must(caps.Get(ctx, id)).ProductVersion(); got != long {
		t.Errorf("product_version = %q", got)
	}
}

// TestSoftwareVersionMigrationKeepsReferences rolls the nodes rebuild back
// and applies it again: dropping nodes must not cascade to the rows that
// reference it.
func TestSoftwareVersionMigrationKeepsReferences(t *testing.T) {
	ctx := context.Background()
	a := dbtest.NewSQLite(t)
	r := Repos{
		Nodes: sqlite.NewNodeRepository(a), Cursors: sqlite.NewCursorRepository(a),
		Caps: sqlite.NewCapabilityRepository(a), Devices: sqlite.NewDeviceRepository(a),
	}
	id := domain.MustNodeID("attic")

	if err := r.Nodes.Create(ctx, domain.NewNode(id, domain.MustNodeName("a"), domain.MustNodeURL("https://x:1"), t0)); err != nil {
		t.Fatal(err)
	}

	spec := domain.DeviceSpec{ID: shared.MustDeviceID("hf"), Name: "HF", Type: "rtl_sdr", Enabled: true, FreqMin: 1, FreqMax: 2, SampleRates: []int64{48_000}}
	if err := r.Devices.Save(ctx, must(domain.NewReportedDevice(id, spec, 0, t0))); err != nil {
		t.Fatal(err)
	}

	ft8 := must(domain.NewCapability("mode:ft8", true, domain.CapabilityOK, "", nil, ""))
	rep := must(domain.NewCapabilityReport(id, "h", "1.0.0", []string{"rx-ctl.v1"}, json.RawMessage(`{}`), json.RawMessage(`{}`), t0, []domain.Capability{ft8}))

	if err := r.Caps.Replace(ctx, rep); err != nil {
		t.Fatal(err)
	}

	boot := must(shared.NewUUIDv7(t0))
	if err := r.Cursors.Advance(ctx, id, boot, 7, t0); err != nil {
		t.Fatal(err)
	}

	for {
		res, err := a.Migrator().Down(ctx)
		if err != nil {
			t.Fatal(err)
		}

		if res.Version <= softwareVersionMigration {
			break
		}
	}

	check := func(step string) {
		t.Helper()

		if _, err := r.Devices.Get(ctx, spec.ID); err != nil {
			t.Errorf("%s: device: %v", step, err)
		}

		if got, err := r.Caps.Get(ctx, id); err != nil || len(got.Capabilities()) != 1 {
			t.Errorf("%s: capabilities = %+v, %v", step, got, err)
		}

		if seq, err := r.Cursors.Last(ctx, id, boot); err != nil || seq != 7 {
			t.Errorf("%s: cursor = %d, %v", step, seq, err)
		}

		var fk int
		if err := a.Reader(ctx).QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
			t.Errorf("%s: foreign_keys = %d, %v", step, fk, err)
		}
	}

	check("down")

	if _, err := a.Migrator().Up(ctx); err != nil {
		t.Fatal(err)
	}

	check("up")

	var fk int
	if err := a.Writer(ctx).QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Errorf("writer foreign_keys = %d, %v", fk, err)
	}
}
