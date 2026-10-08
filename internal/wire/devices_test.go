package wire_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// seedDevices registers node attic with a reported device (hf) and a device
// its node no longer reports (vhf).
func seedDevices(t *testing.T, h *adminHub) {
	t.Helper()

	ctx := context.Background()
	now := time.Now()
	node := griddomain.MustNodeID("attic")

	if err := gridsqlite.NewNodeRepository(h.db).Create(ctx, griddomain.NewNode(node, griddomain.MustNodeName("Attic"),
		griddomain.MustNodeURL("https://attic:8074"), now)); err != nil {
		t.Fatal(err)
	}

	repo := gridsqlite.NewDeviceRepository(h.db)

	for i, id := range []string{"hf", "vhf"} {
		spec := griddomain.DeviceSpec{ID: shared.MustDeviceID(id), Name: strings.ToUpper(id) + " receiver", Type: "rtl_sdr",
			Enabled: true, FreqMin: 100_000, FreqMax: 30_000_000, SampleRates: []int64{2_048_000}, OperatorCanRetune: true}
		if id == "hf" {
			spec.Config = &griddomain.DeviceConfig{RFGain: "28.5", PPM: -3, BiasTee: true, DirectSampling: "q", IQSwap: true, LFOOffset: -120_000_000}
		}

		d, err := griddomain.NewReportedDevice(node, spec, i, now)
		if err != nil {
			t.Fatal(err)
		}

		if id == "vhf" {
			d.MarkUnavailable(now)
		}

		if err := repo.Save(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDevicePages(t *testing.T) {
	h := newAdminHub(t, nil)
	seedDevices(t, h)

	op := h.browser("op")

	res, body := op.do(http.MethodGet, "/admin/devices", "", "", nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `href="/admin/devices/hf"`) ||
		!strings.Contains(string(body), "no longer reported since") || strings.Contains(string(body), `href="/admin/site"`) {
		t.Fatalf("operator list = %d %s", res.StatusCode, body)
	}

	res, body = op.do(http.MethodGet, "/admin/devices/vhf", "", "", nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "Defined in the node config of Attic") ||
		strings.Contains(string(body), "Forget this device") {
		t.Errorf("operator detail = %d %s", res.StatusCode, body)
	}

	lis, admin := h.browser("lis"), h.browser("root")

	if res, _ := lis.do(http.MethodGet, "/admin/devices", "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("listener list = %d", res.StatusCode)
	}

	// ADM-007: the list columns; attic never connected, its rows say so.
	_, body = op.do(http.MethodGet, "/admin/devices", "", "", nil)
	for _, want := range []string{">Device id<", ">Active preset<", ">Listeners<", "(node offline)", `class="border-b border-border text-fg-muted"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("operator list lacks %s", want)
		}
	}

	// SRC-022: the reported driver values, read-only.
	_, body = op.do(http.MethodGet, "/admin/devices/hf", "", "", nil)
	for _, want := range []string{"Set in node config", "28.5 dB", "-3 ppm", "Q branch", "IQ swap", "−120 MHz", "global listen policy"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("operator detail lacks %s", want)
		}
	}

	if strings.Contains(string(body), "/admin/devices/hf/log") {
		t.Error("device log offered to an operator")
	}

	if _, body := op.do(http.MethodGet, "/admin/devices/vhf", "", "", nil); !strings.Contains(string(body), "does not report the driver values") {
		t.Error("device without reported values")
	}

	// SRC-005: the device log is for admins.
	for b, want := range map[*browser]int{op: http.StatusForbidden, lis: http.StatusForbidden, admin: http.StatusOK} {
		res, body := b.do(http.MethodGet, "/admin/devices/hf/log", "", "", nil)
		if res.StatusCode != want {
			t.Errorf("log = %d, want %d", res.StatusCode, want)
		}

		if want == http.StatusOK && (!strings.Contains(string(body), `data-msdr-topics="device_log:device=hf"`) ||
			!strings.Contains(string(body), "No record yet")) {
			t.Errorf("log page = %s", body)
		}
	}

	if _, body := admin.do(http.MethodGet, "/admin/devices/hf", "", "", nil); !strings.Contains(string(body), `href="/admin/devices/hf/log"`) {
		t.Error("device log not linked for an admin")
	}

	if res, _ := admin.do(http.MethodGet, "/admin/devices/nope/log", "", "", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("log of an unknown device = %d", res.StatusCode)
	}

	if res, _ := op.do(http.MethodPost, "/admin/devices/vhf/forget", "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("operator forget = %d", res.StatusCode)
	}

	if res, _ := op.do(http.MethodGet, "/admin/devices/nope", "", "", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown device = %d", res.StatusCode)
	}

	if _, body := admin.do(http.MethodGet, "/admin/devices/vhf", "", "", nil); !strings.Contains(string(body), "Forget this device") {
		t.Error("admin detail of a missing device without Forget")
	}

	if _, body := admin.do(http.MethodGet, "/admin/devices/hf", "", "", nil); strings.Contains(string(body), "Forget this device") {
		t.Error("Forget offered for a reported device")
	}

	if res, body := admin.do(http.MethodPost, "/admin/devices/hf/forget", "", "", map[string]string{"HX-Request": "true"}); res.StatusCode != http.StatusConflict ||
		!strings.Contains(string(body), "still reported") {
		t.Errorf("forget a reported device = %d", res.StatusCode)
	}

	res, _ = admin.do(http.MethodPost, "/admin/devices/vhf/forget", "", "", map[string]string{"HX-Request": "true"})
	if res.StatusCode != http.StatusNoContent || res.Header.Get("HX-Redirect") != "/admin/devices" {
		t.Fatalf("forget = %d %v", res.StatusCode, res.Header)
	}

	if res, _ := admin.do(http.MethodGet, "/admin/devices/vhf", "", "", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("forgotten device = %d", res.StatusCode)
	}

	if n := h.count("SELECT count(*) FROM audit_log WHERE action = 'device.forget' AND target_type = 'device' AND target_id = 'vhf'"); n != 1 {
		t.Errorf("audit rows = %d", n)
	}
}
