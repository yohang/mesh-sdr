package app_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/files/app"
	"github.com/yohang/mesh-sdr/internal/files/domain"
	"github.com/yohang/mesh-sdr/internal/files/infra/imaging"
	"github.com/yohang/mesh-sdr/internal/files/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/shared/audit"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func TestBranding(t *testing.T) {
	ctx := context.Background()
	a := dbtest.NewSQLite(t)
	au := &audit.Records{}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	b := app.NewBranding(app.BrandingDeps{
		Repo: sqlite.NewFiles(a), Tx: a, Processor: imaging.NewProcessor(), IDs: shared.NewUUIDv7Generator(),
		Audit: au, Now: func() time.Time { return now },
	})

	if _, err := b.Current(ctx, domain.SlotAvatar); !errors.Is(err, domain.ErrImageNotSet) {
		t.Fatalf("no avatar: %v", err)
	}

	first, err := b.Upload(ctx, shared.UUID{}, domain.SlotAvatar, pngOf(t, 64, 64))
	if err != nil {
		t.Fatal(err)
	}

	if first.Name() != "avatar-261006-120000.png" || first.MIME() != domain.MIMEPNG || first.Width() != 64 {
		t.Errorf("avatar = %s %s %d", first.Name(), first.MIME(), first.Width())
	}

	second, err := b.Upload(ctx, shared.UUID{}, domain.SlotAvatar, pngOf(t, 32, 16))
	if err != nil {
		t.Fatal(err)
	}

	f, data, err := b.Content(ctx, domain.SlotAvatar)
	if err != nil || f.ID() != second.ID() || int64(len(data)) != f.Size() {
		t.Fatalf("current = %v, %v", f, err)
	}

	var n int
	if err := a.Reader(ctx).QueryRowContext(ctx, "SELECT count(*) FROM files").Scan(&n); err != nil || n != 1 {
		t.Errorf("files rows = %d, %v (the previous avatar must be replaced)", n, err)
	}

	if _, err := b.Upload(ctx, shared.UUID{}, domain.SlotAvatar, make([]byte, domain.MaxAvatarUpload+1)); !errors.Is(err, domain.ErrImageTooLarge) {
		t.Errorf("oversized: %v", err)
	}

	if _, err := b.Upload(ctx, shared.UUID{}, domain.SlotPanorama, []byte("<svg/>")); !errors.Is(err, domain.ErrUnsupportedImage) {
		t.Errorf("svg: %v", err)
	}

	if removed, err := b.Remove(ctx, domain.SlotAvatar); err != nil || !removed {
		t.Errorf("remove = %v, %v", removed, err)
	}

	if removed, err := b.Remove(ctx, domain.SlotAvatar); err != nil || removed {
		t.Errorf("second remove = %v, %v", removed, err)
	}

	if len(*au) != 3 || (*au)[2].Action != app.ActionImageRemove || (*au)[0].TargetID != "avatar" {
		t.Errorf("audit = %+v", *au)
	}
}
