package wire

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/http/api"
	"github.com/yohang/mesh-sdr/internal/http/api/apitest"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// seedListenDevices registers node attic with an anonymous-listenable
// device (open) and a registered-only one (members).
func seedListenDevices(t *testing.T, h *contractHub) {
	t.Helper()

	ctx := context.Background()
	now := time.Now()
	node := domain.MustNodeID("attic")

	if err := gridsqlite.NewNodeRepository(h.adapter).Create(ctx, domain.NewNode(node, domain.MustNodeName("Attic"),
		domain.MustNodeURL("https://attic:8074"), now)); err != nil {
		t.Fatal(err)
	}

	for i, d := range []struct{ id, policy string }{{"open", "anonymous"}, {"members", "registered"}} {
		dev, err := domain.NewReportedDevice(node, domain.DeviceSpec{
			ID: shared.MustDeviceID(d.id), Name: d.id, Type: "rtl_sdr", Enabled: true, FreqMin: 24_000_000, FreqMax: 1_700_000_000,
			SampleRates: []int64{2_048_000}, ListenPolicy: d.policy,
		}, i, now)
		if err != nil {
			t.Fatal(err)
		}

		if err := gridsqlite.NewDeviceRepository(h.adapter).Save(ctx, dev); err != nil {
			t.Fatal(err)
		}
	}
}

// TestBookmarksAPIContract covers GET /bookmarks and GET /bandplan against
// openapi.yaml: the listen policy of the device decides who reads its
// bookmarks (BMK-001), pack and hub rows are returned with their origin.
func TestBookmarksAPIContract(t *testing.T) {
	v, err := apitest.New(api.Spec())
	if err != nil {
		t.Fatal(err)
	}

	h := newContractHub(t, v, map[string]identitydomain.Role{"lis": identitydomain.RoleListener, "op": identitydomain.RoleOperator})
	seedListenDevices(t, h)

	if _, err := SyncBookmarks(context.Background(), h.adapter, quiet); err != nil {
		t.Fatal(err)
	}

	op := h.signedIn("op", 192)
	if status := op.form("/bookmarks/manage", url.Values{
		"name": {"Club net"}, "frequency": {"446006250"}, "modulation": {"nfm"}, "scope": {"all"}, "scannable": {"on"},
	}); status != http.StatusSeeOther {
		t.Fatalf("operator create = %d", status)
	}

	anon, lis := h.client(192), h.signedIn("lis", 192)
	pmr := "/bookmarks?device_id=open&from=446000000&to=446200000"

	status, body := anon.do(http.MethodGet, pmr, nil)
	list, _ := body["bookmarks"].([]any)

	if status != http.StatusOK || body["region"] != "r1" || len(list) != 17 {
		t.Fatalf("anonymous GET %s = %d, %d bookmarks: %v", pmr, status, len(list), body)
	}

	// The hub bookmark comes first, before the pack bookmark on the same
	// frequency.
	first, _ := list[0].(map[string]any)
	second, _ := list[1].(map[string]any)

	if first["name"] != "Club net" || first["origin"] != "db" || second["name"] != "PMR1" || second["origin"] != "builtin" {
		t.Errorf("first bookmarks = %v, %v", first, second)
	}

	for _, tt := range []struct {
		client *apiClient
		path   string
		want   int
		code   string
	}{
		{anon, "/bookmarks?device_id=members", http.StatusNotFound, "device_not_found"},
		{anon, "/bookmarks?device_id=nope", http.StatusNotFound, "device_not_found"},
		{lis, "/bookmarks?device_id=members", http.StatusOK, ""},
		{anon, "/bookmarks?device_id=open&from=10&to=5", http.StatusUnprocessableEntity, "invalid_range"},
		{anon, "/bookmarks", http.StatusBadRequest, ""},
	} {
		if status, body := tt.client.do(http.MethodGet, tt.path, nil); status != tt.want || (tt.code != "" && body["code"] != tt.code) {
			t.Errorf("GET %s = %d %v, want %d %s", tt.path, status, body["code"], tt.want, tt.code)
		}
	}

	status, body = anon.do(http.MethodGet, "/bandplan?from=7000000&to=7100000", nil)
	bands, _ := body["bands"].([]any)
	dials, _ := body["dials"].([]any)

	if status != http.StatusOK || body["region"] != "r1" || len(bands) != 1 || len(dials) == 0 {
		t.Fatalf("GET /bandplan = %d %v", status, body)
	}

	if b, _ := bands[0].(map[string]any); b["name"] != "40m" {
		t.Errorf("band = %v", b)
	}
}
