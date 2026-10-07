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

	if res, _ := h.browser("lis").do(http.MethodGet, "/admin/devices", "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("listener list = %d", res.StatusCode)
	}

	if res, _ := op.do(http.MethodPost, "/admin/devices/vhf/forget", "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("operator forget = %d", res.StatusCode)
	}

	if res, _ := op.do(http.MethodGet, "/admin/devices/nope", "", "", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown device = %d", res.StatusCode)
	}

	admin := h.browser("root")

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
