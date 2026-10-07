package wire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	filesapp "github.com/yohang/mesh-sdr/internal/files/app"
	filesdomain "github.com/yohang/mesh-sdr/internal/files/domain"
	"github.com/yohang/mesh-sdr/internal/files/infra/imaging"
	filessqlite "github.com/yohang/mesh-sdr/internal/files/infra/sqlite"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// branding builds the receiver images use case (ADM-004).
func branding(adapter *db.DB, audit identitydomain.AuditLog) *filesapp.Branding {
	return filesapp.NewBranding(filesapp.BrandingDeps{
		Repo: filessqlite.NewFiles(adapter), Tx: adapter, Processor: imaging.NewProcessor(),
		IDs: shared.NewUUIDv7Generator(), Audit: imageAuditor{log: audit}, Now: time.Now,
	})
}

// imageAuditor writes receiver image changes to identity's audit_log.
type imageAuditor struct{ log identitydomain.AuditLog }

// RecordImage implements filesapp.Auditor.
func (a imageAuditor) RecordImage(ctx context.Context, r filesapp.AuditRecord) error {
	actor := identitydomain.SystemActor()

	if !r.Actor.User.IsZero() {
		id, err := identitydomain.NewUserID(r.Actor.User)
		if err != nil {
			return fmt.Errorf("audit actor: %w", err)
		}

		actor = identitydomain.UserActor(id, r.Actor.IP)
	}

	e, err := identitydomain.NewAuditEntry(r.At, actor, r.Action, identitydomain.ResultOK)
	if err != nil {
		return fmt.Errorf("audit entry: %w", err)
	}

	e = e.WithTarget("receiver_image", r.Slot).WithRequestID(r.Actor.RequestID)
	if r.After != nil {
		e = e.WithAfter(r.After)
	}

	return a.log.Append(ctx, e)
}

// filesActor returns the signed-in user of a request as a files actor.
func filesActor(ctx context.Context) filesapp.Actor {
	a := settingsActor(ctx)

	return filesapp.Actor{User: a.User, IP: a.IP, RequestID: a.RequestID}
}

// stationImages tells the shell whether a receiver image is set. An image
// that cannot be read is treated as not set (the Receiver page then links
// no image) and logged at Warn.
type stationImages struct {
	b      *filesapp.Branding
	logger *slog.Logger
}

// HasImage implements shell/app.StationImages.
func (s stationImages) HasImage(ctx context.Context, slot string) bool {
	sl, err := filesdomain.ParseSlot(slot)
	if err != nil {
		s.logger.WarnContext(ctx, "unknown receiver image slot", slog.String("slot", slot), slog.Any("error", err))

		return false
	}

	f, err := s.b.Current(ctx, sl)

	switch {
	case errors.Is(err, filesdomain.ErrImageNotSet):
		return false
	case err != nil:
		s.logger.WarnContext(ctx, "receiver image unavailable", slog.String("slot", slot), slog.Any("error", err))

		return false
	}

	return f != nil
}
