package wire

import (
	"bytes"
	"context"
	"crypto/sha256"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/files"
	"github.com/yohang/mesh-sdr/internal/grid/agent"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/http/api"
	"github.com/yohang/mesh-sdr/internal/http/api/apitest"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// TestStationImages: an image that is not set is simply absent; an image
// that cannot be read is absent too, and logged at Warn.
func TestStationImages(t *testing.T) {
	adapter := dbtest.NewSQLite(t)

	var logs bytes.Buffer

	s := stationImages{b: branding(adapter, newAuditAppender(adapter, time.Now)), logger: slog.New(slog.NewTextHandler(&logs, nil))}
	ctx := context.Background()

	if s.HasImage(ctx, "avatar") || logs.Len() != 0 {
		t.Fatalf("unset avatar: has image or logged %q", logs.String())
	}

	if s.HasImage(ctx, "logo") || !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("unknown slot not logged: %q", logs.String())
	}

	logs.Reset()

	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}

	if s.HasImage(ctx, "panorama") || !strings.Contains(logs.String(), "receiver image unavailable") {
		t.Errorf("read failure not logged: %q", logs.String())
	}
}

// pngImage is a small PNG image.
func pngImage(t *testing.T, w, h int) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

// storeNodeFile stores a complete SSTV image of device, as the attic node
// sends it, and returns its id.
func storeNodeFile(t *testing.T, adapter *db.DB, device string) string {
	t.Helper()

	ctx := context.Background()
	content := pngImage(t, 64, 48)
	sum := sha256.Sum256(content)

	id, err := shared.NewUUIDv7(time.Now())
	if err != nil {
		t.Fatal(err)
	}

	in := files.Incoming{
		ID: id, Node: "attic", Kind: files.KindSSTV, MIME: files.MIMEPNG, Size: int64(len(content)), SHA256: sum[:],
		DeviceID: shared.MustDeviceID(device), Mode: "sstv", FrequencyHz: 14_230_000, ReceivedStart: time.Now().UTC(),
	}
	ingest := files.NewIngest(files.IngestDeps{Repo: files.NewFiles(adapter), Tx: adapter, Processor: files.NewProcessor(), Logger: quiet})

	err = adapter.WithinTx(ctx, func(ctx context.Context) error {
		if err := ingest.Begin(ctx, in, nil, time.Now()); err != nil {
			return err
		}

		if err := ingest.Chunk(ctx, "attic", id, 0, content, time.Now()); err != nil {
			return err
		}

		return ingest.End(ctx, "attic", id)
	})
	if err != nil {
		t.Fatal(err)
	}

	ingest.Finalize(ctx, "attic")

	return id.String()
}

// withOutbox hands the node's file outbox to the test.
func withOutbox(out **agent.Outbox) NodeOption {
	return func(o *nodeOptions) { o.outbox = func(b *agent.Outbox) { *out = b } }
}

// TestFilesFromNode covers FIL-005 end to end, over the control channel
// between a hub and a node process: the node sends a PNG, the hub stores it
// complete with its reception metadata and acknowledges it, and the node
// deletes its copy.
func TestFilesFromNode(t *testing.T) {
	e := newGridEnv(t, fastTimings())
	e.nodeCfg.Node.RuntimeDir = t.TempDir()
	if err := os.Chmod(e.nodeCfg.Node.RuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	var outbox *agent.Outbox

	e.enrollNode(t, fakeProber{}, withOutbox(&outbox))

	ctx := context.Background()

	eventually(t, "device registry", 15*time.Second, func() bool {
		_, err := e.g.devices.Get(ctx, "hf")

		return err == nil
	})

	src := filepath.Join(e.nodeCfg.Node.RuntimeDir, "sstv.png")
	writeFile(t, src, string(pngImage(t, 320, 256)), 0o600)

	start := time.Date(2026, 10, 6, 14, 32, 5, 0, time.UTC)
	meta := agent.FileMeta{
		Kind: agent.FileKindSSTV, DeviceID: "hf", Mode: "sstv", FrequencyHz: 14_230_000, ReceivedStart: start,
		ReceivedEnd: start.Add(110 * time.Second), Metadata: map[string]any{"sstv_mode": "Robot 36", "vis_code": 8},
	}

	if err := outbox.SendFile(ctx, meta, src); err != nil {
		t.Fatal(err)
	}

	repo := files.NewFiles(e.adapter)

	var list []files.Entry

	eventually(t, "file stored", 15*time.Second, func() bool {
		var err error

		list, err = repo.List(ctx, files.Filter{}, files.Access{}, 0, 10)

		return err == nil && len(list) == 1
	})

	f := list[0]
	if f.Name != "SSTV-261006-143205-14230.png" || f.NodeID != "attic" || f.DeviceID != "hf" || f.Width != 320 ||
		!f.ReceivedStart.Equal(start) || f.Metadata["sstv_mode"] != "Robot 36" || !f.HasThumbnail {
		t.Errorf("stored file = %+v", f)
	}

	eventually(t, "node copy deleted", 15*time.Second, func() bool {
		n, size := outbox.Pending()

		entries, _ := os.ReadDir(filepath.Join(e.nodeCfg.Node.RuntimeDir, "outbox"))

		return n == 0 && size == 0 && len(entries) == 0
	})

	// A file of a device the node does not have is refused.
	other := filepath.Join(e.nodeCfg.Node.RuntimeDir, "other.png")
	writeFile(t, other, string(pngImage(t, 8, 8)), 0o600)
	meta.DeviceID = "vhf"

	if err := outbox.SendFile(ctx, meta, other); err != nil {
		t.Fatal(err)
	}

	eventually(t, "refused file acknowledged", 15*time.Second, func() bool {
		n, _ := outbox.Pending()

		return n == 0
	})

	if list, _ := repo.List(ctx, files.Filter{}, files.Access{}, 0, 10); len(list) != 1 {
		t.Errorf("files = %d, want the first one only", len(list))
	}
}

// fetch sends a GET to a page or API path and returns the response, with
// its body read.
func (c *apiClient) fetch(path string, header map[string]string) (*http.Response, []byte) {
	c.h.t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.h.url+path, nil)
	if err != nil {
		c.h.t.Fatal(err)
	}

	req.Header.Set("X-Forwarded-For", c.addr)

	for k, v := range header {
		req.Header.Set(k, v)
	}

	res, err := c.http.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}

	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		c.h.t.Fatal(err)
	}

	return res, body
}

// TestFilesPages covers FIL-001, FIL-002, FIL-003 and FIL-007 in the hub:
// the gallery and detail page, the download with its headers, the rights
// of each role (listen policy, ADR 0026) and the audited deletions.
func TestFilesPages(t *testing.T) {
	v, err := apitest.New(api.Spec())
	if err != nil {
		t.Fatal(err)
	}

	h := newContractHub(t, v, map[string]identitydomain.Role{
		"listener": identitydomain.RoleListener, "operator": identitydomain.RoleOperator, "admin": identitydomain.RoleAdmin,
	})
	ctx := context.Background()
	now := time.Now()

	anon := h.client(10)
	listener := h.signedIn("listener", 10)
	operator := h.signedIn("operator", 10)
	admin := h.signedIn("admin", 10)

	if status := admin.form("/admin/nodes", url.Values{"id": {"attic"}, "url": {"https://10.8.0.12:8074"}}); status != http.StatusOK {
		t.Fatalf("add a node = %d", status)
	}

	// Two devices of the attic node: hf follows the global policy, vhf
	// needs a signed-in listener.
	for _, d := range []domain.DeviceSpec{
		{ID: shared.MustDeviceID("hf"), Name: "HF", Type: "rtl_sdr", Enabled: true, FreqMin: 100_000, FreqMax: 30_000_000, SampleRates: []int64{2_048_000}},
		{ID: shared.MustDeviceID("vhf"), Name: "VHF", Type: "rtl_sdr", Enabled: true, FreqMin: 100_000, FreqMax: 30_000_000, SampleRates: []int64{2_048_000}, ListenPolicy: "registered"},
	} {
		dev, err := domain.NewReportedDevice(domain.MustNodeID("attic"), d, 0, now)
		if err != nil {
			t.Fatal(err)
		}

		if err := gridsqlite.NewDeviceRepository(h.adapter).Save(ctx, dev); err != nil {
			t.Fatal(err)
		}
	}

	hf := storeNodeFile(t, h.adapter, "hf")
	vhf := storeNodeFile(t, h.adapter, "vhf")

	// The gallery: a visitor sees the files of hf only, a listener both.
	if res, body := anon.fetch("/files", nil); res.StatusCode != http.StatusOK || !strings.Contains(string(body), hf) ||
		strings.Contains(string(body), vhf) || !strings.Contains(string(body), "14.230 MHz") || !strings.Contains(string(body), " UTC") {
		t.Errorf("anonymous gallery = %d", res.StatusCode)
	}

	if _, body := listener.fetch("/files", nil); !strings.Contains(string(body), hf) || !strings.Contains(string(body), vhf) {
		t.Error("listener gallery misses a file")
	} else if !strings.Contains(string(body), `<option value="hf">HF</option>`) || !strings.Contains(string(body), `<option value="vhf">VHF</option>`) {
		t.Error("device filter shows the device ids, not their names")
	}

	// Filters: the device, a frequency range outside the files.
	if _, body := listener.fetch("/files?device=vhf", nil); strings.Contains(string(body), hf) || !strings.Contains(string(body), vhf) {
		t.Error("device filter")
	}

	if _, body := listener.fetch("/files?freq_min=144000", nil); strings.Contains(string(body), hf) || strings.Contains(string(body), vhf) {
		t.Error("frequency filter")
	}

	// The detail page, and the content with its headers (FIL-002, SR-30).
	if res, body := anon.fetch("/files/"+hf, nil); res.StatusCode != http.StatusOK || !strings.Contains(string(body), "/receiver/attic/hf?f=14230000") ||
		!strings.Contains(string(body), "/decodes?device=hf&amp;from=") ||
		strings.Contains(string(body), "Delete the file") {
		t.Errorf("anonymous detail = %d", res.StatusCode)
	}

	if _, body := operator.fetch("/files/"+hf, nil); !strings.Contains(string(body), "Delete the file") {
		t.Error("the operator has no delete form")
	}

	res, body := anon.fetch("/api/v1/files/"+hf+"/content", nil)
	etag := res.Header.Get("ETag")

	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/png" || etag == "" ||
		res.Header.Get("X-Content-Type-Options") != "nosniff" ||
		!regexp.MustCompile(`^attachment; filename="SSTV-\d{6}-\d{6}-14230\.png"$`).MatchString(res.Header.Get("Content-Disposition")) ||
		!bytes.HasPrefix(body, []byte("\x89PNG")) {
		t.Errorf("content = %d %v", res.StatusCode, res.Header)
	}

	if res, _ := anon.fetch("/api/v1/files/"+hf+"/content", map[string]string{"If-None-Match": etag}); res.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match = %d", res.StatusCode)
	}

	// A file the caller may not see is not found, on every route.
	for _, path := range []string{"/files/" + vhf, "/api/v1/files/" + vhf + "/content", "/api/v1/files/" + vhf + "/thumbnail", "/files/not-an-id", "/api/v1/files/not-an-id/content"} {
		if res, _ := anon.fetch(path, nil); res.StatusCode != http.StatusNotFound {
			t.Errorf("anonymous %s = %d", path, res.StatusCode)
		}
	}

	if res, _ := listener.fetch("/api/v1/files/"+vhf+"/content", nil); res.StatusCode != http.StatusOK {
		t.Errorf("listener content of vhf = %d", res.StatusCode)
	}

	// Deletion (FIL-003): not for visitors nor listeners; an operator
	// deletes one file, audited.
	if status := anon.form("/files/"+hf+"/delete", nil); status != http.StatusSeeOther {
		t.Errorf("anonymous delete = %d (sign-in redirect)", status)
	}

	if status := listener.form("/files/"+hf+"/delete", nil); status != http.StatusForbidden {
		t.Errorf("listener delete = %d", status)
	}

	if status := operator.form("/files/delete", nil); status != http.StatusForbidden {
		t.Errorf("operator bulk delete = %d", status)
	}

	if status := operator.form("/files/"+hf+"/delete", nil); status != http.StatusSeeOther {
		t.Errorf("operator delete = %d", status)
	}

	if status := operator.form("/files/"+hf+"/delete", nil); status != http.StatusNotFound {
		t.Errorf("second delete = %d", status)
	}

	// An admin deletes by filter.
	storeNodeFile(t, h.adapter, "hf")

	// An invalid filter, or no filter without all=1, is refused: it would
	// widen the deletion.
	for _, values := range []url.Values{{"device": {"../vhf"}}, {"media": {"video"}}, {"mode": {"SSTV!"}}, {"freq_min": {"x"}}, nil} {
		if status := admin.form("/files/delete", values); status != http.StatusUnprocessableEntity {
			t.Errorf("bulk delete %v = %d, want 422", values, status)
		}
	}

	if status := admin.form("/files/delete", url.Values{"device": {"vhf"}}); status != http.StatusSeeOther {
		t.Errorf("admin bulk delete = %d", status)
	}

	count := func(q string) int {
		var n int
		if err := h.adapter.Reader(ctx).QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}

		return n
	}

	if n := count("SELECT count(*) FROM files WHERE kind = 'sstv'"); n != 1 {
		t.Errorf("files left = %d, want the second hf file", n)
	}

	if n := count("SELECT count(*) FROM audit_log WHERE action = 'file.delete' AND target_id = '" + hf + "'"); n != 1 {
		t.Errorf("delete audit = %d", n)
	}

	if n := count("SELECT count(*) FROM audit_log WHERE action = 'file.delete_bulk'"); n != 1 {
		t.Errorf("bulk delete audit = %d", n)
	}

	if status := admin.form("/files/delete", url.Values{"all": {"1"}}); status != http.StatusSeeOther || count("SELECT count(*) FROM files WHERE kind = 'sstv'") != 0 {
		t.Errorf("delete every file = %d", status)
	}

	// Under the registered policy, visitors are sent to sign in and see
	// no file nor the Files section.
	if status := admin.form("/admin/access", url.Values{"section": {"listening"}, "listen_policy": {"registered"}, "version.listen_policy": {"0"}}); status >= http.StatusBadRequest {
		t.Fatalf("listen policy = %d", status)
	}

	if res, _ := anon.fetch("/files", nil); res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/login") {
		t.Errorf("anonymous gallery under the registered policy = %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	if _, body := anon.fetch("/policy", nil); strings.Contains(string(body), `data-section="files"`) {
		t.Error("the Files section is offered to a visitor under the registered policy")
	}

	if _, body := listener.fetch("/policy", nil); !strings.Contains(string(body), `data-section="files"`) {
		t.Error("the Files section is not offered to a listener")
	}
}
