package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/settings/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Fixture is a fresh, migrated database: the repository and the id of an
// existing user (settings.updated_by references users).
type Fixture struct {
	Settings domain.Repository
	User     shared.UUID
	Tx       func(ctx context.Context, fn func(ctx context.Context) error) error
}

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func TestSettings(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		f := open(t)
		ctx := context.Background()

		rows, err := f.Settings.List(ctx)
		if err != nil || len(rows) != 0 {
			t.Fatalf("list = %v, %v", rows, err)
		}

		got, err := f.Settings.Get(ctx, "ui.theme_mode")
		if err != nil || got != nil {
			t.Fatalf("get = %v, %v", got, err)
		}

		if rev, err := f.Settings.Revision(ctx); err != nil || rev != 0 {
			t.Fatalf("revision = %d, %v", rev, err)
		}
	})

	t.Run("save, replace, delete", func(t *testing.T) {
		f := open(t)
		ctx := context.Background()
		key := "receiver.gps"

		rev, err := f.Settings.NextRevision(ctx)
		if err != nil || rev != 1 {
			t.Fatalf("next revision = %d, %v", rev, err)
		}

		s, err := domain.NewSetting(key, domain.MustValue(`{"lat":50.5,"lon":3}`), rev, f.User, t0)
		if err != nil {
			t.Fatal(err)
		}

		if err := f.Settings.Save(ctx, s); err != nil {
			t.Fatal(err)
		}

		got, err := f.Settings.Get(ctx, key)
		if err != nil || got == nil {
			t.Fatalf("get = %v, %v", got, err)
		}

		if got.Value().String() != `{"lat":50.5,"lon":3}` || got.Version() != 1 || got.UpdatedBy() != f.User || !got.UpdatedAt().Equal(t0) {
			t.Errorf("row = %s v%d by %s at %s", got.Value(), got.Version(), got.UpdatedBy(), got.UpdatedAt())
		}

		rev, _ = f.Settings.NextRevision(ctx)
		if err := got.Replace(domain.MustValue(`{"lat":1,"lon":2}`), rev, shared.UUID{}, t0.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}

		if err := f.Settings.Save(ctx, got); err != nil {
			t.Fatal(err)
		}

		rows, err := f.Settings.List(ctx)
		if err != nil || len(rows) != 1 || rows[0].Version() != 2 || !rows[0].UpdatedBy().IsZero() {
			t.Fatalf("list = %v, %v", rows, err)
		}

		if err := f.Settings.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}

		if err := f.Settings.Delete(ctx, key); err != nil {
			t.Fatalf("second delete: %v", err)
		}

		if got, _ := f.Settings.Get(ctx, key); got != nil {
			t.Error("deleted row still there")
		}

		if rev, _ := f.Settings.Revision(ctx); rev != 2 {
			t.Errorf("revision = %d after a delete, want 2", rev)
		}
	})

	t.Run("rollback", func(t *testing.T) {
		f := open(t)
		ctx := context.Background()

		_ = f.Tx(ctx, func(ctx context.Context) error {
			rev, err := f.Settings.NextRevision(ctx)
			if err != nil {
				t.Fatal(err)
			}

			s, _ := domain.NewSetting("listen_policy", domain.MustValue(`"registered"`), rev, shared.UUID{}, t0)
			if err := f.Settings.Save(ctx, s); err != nil {
				t.Fatal(err)
			}

			return context.Canceled
		})

		if rev, _ := f.Settings.Revision(ctx); rev != 0 {
			t.Errorf("revision = %d after rollback", rev)
		}

		if rows, _ := f.Settings.List(ctx); len(rows) != 0 {
			t.Errorf("rows after rollback: %v", rows)
		}
	})
}
