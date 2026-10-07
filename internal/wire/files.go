package wire

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/yohang/mesh-sdr/internal/shared/audit"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/files"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// branding builds the receiver images use case (ADM-004).
func branding(adapter *db.DB, audit audit.Appender) *files.Branding {
	return files.NewBranding(files.BrandingDeps{
		Repo: files.NewFiles(adapter), Tx: adapter, Processor: files.NewProcessor(),
		IDs: shared.NewUUIDv7Generator(), Audit: audit, Now: time.Now,
	})
}

// stationImages tells the shell whether a receiver image is set. An image
// that cannot be read is treated as not set (the Receiver page then links
// no image) and logged at Warn.
type stationImages struct {
	b      *files.Branding
	logger *slog.Logger
}

// HasImage implements shell/app.StationImages.
func (s stationImages) HasImage(ctx context.Context, slot string) bool {
	sl, err := files.ParseSlot(slot)
	if err != nil {
		s.logger.WarnContext(ctx, "unknown receiver image slot", slog.String("slot", slot), slog.Any("error", err))

		return false
	}

	f, err := s.b.Current(ctx, sl)

	switch {
	case errors.Is(err, files.ErrImageNotSet):
		return false
	case err != nil:
		s.logger.WarnContext(ctx, "receiver image unavailable", slog.String("slot", slot), slog.Any("error", err))

		return false
	}

	return f != nil
}
