package wire

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	identitysqlite "github.com/yohang/mesh-sdr/internal/identity/infra/sqlite"
)

// TestStationImages: an image that is not set is simply absent; an image
// that cannot be read is absent too, and logged at Warn.
func TestStationImages(t *testing.T) {
	adapter := dbtest.NewSQLite(t)

	var logs bytes.Buffer

	s := stationImages{b: branding(adapter, identitysqlite.NewAuditLog(adapter)), logger: slog.New(slog.NewTextHandler(&logs, nil))}
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
