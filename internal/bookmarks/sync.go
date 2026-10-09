package bookmarks

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
)

// SyncResult counts what a pack sync did.
type SyncResult struct {
	Inserted, Updated, Unchanged, Deleted int
	// Skipped counts the pack bookmarks not stored because a hub bookmark
	// for all devices has the same name, frequency and modulation (hub
	// rows win).
	Skipped int
}

// Sync stores the embedded packs as the builtin rows of d at now (BMK-002,
// `meshsdr hub migrate`): it inserts the new pack bookmarks, updates the
// changed ones, deletes the builtin rows no pack holds any more, in one
// transaction. It is idempotent: a second run changes nothing. Pack ids
// are UUIDv5 of the unique key, so they are stable. Hub rows (origin db)
// are never touched.
func Sync(ctx context.Context, d *db.DB, now time.Time, logger *slog.Logger) (SyncResult, error) {
	packs, err := LoadPacks()
	if err != nil {
		return SyncResult{}, fmt.Errorf("bookmark packs: %w", err)
	}

	s := syncer{repo: NewRepository(d), logger: logger, now: now.UTC().Truncate(time.Millisecond)}

	var res SyncResult

	err = d.WithinTx(ctx, func(ctx context.Context) error {
		res = SyncResult{}

		current, err := s.repo.ByOrigin(ctx, OriginBuiltin)
		if err != nil {
			return err
		}

		byID := make(map[string]*Bookmark, len(current))
		for _, b := range current {
			byID[b.ID().String()] = b
		}

		for _, e := range packs.entries {
			n, err := s.entry(ctx, e, byID)
			if err != nil {
				return err
			}

			switch n {
			case syncInserted:
				res.Inserted++
			case syncUpdated:
				res.Updated++
			case syncUnchanged:
				res.Unchanged++
			case syncSkipped:
				res.Skipped++
			}

			delete(byID, e.id().String())
		}

		for _, b := range byID {
			if err := s.repo.Delete(ctx, b.ID(), OriginBuiltin); err != nil {
				return err
			}

			res.Deleted++
		}

		return nil
	})
	if err != nil {
		return SyncResult{}, fmt.Errorf("sync bookmark packs: %w", err)
	}

	logger.InfoContext(ctx, "bookmark packs synced",
		slog.Int("packs", len(packs.files)), slog.Int("pack_bookmarks", packs.Len()),
		slog.Int("inserted", res.Inserted), slog.Int("updated", res.Updated), slog.Int("unchanged", res.Unchanged),
		slog.Int("deleted", res.Deleted), slog.Int("skipped", res.Skipped))

	return res, nil
}

type syncOutcome int

const (
	syncUnchanged syncOutcome = iota
	syncInserted
	syncUpdated
	syncSkipped
)

// syncer stores pack entries.
type syncer struct {
	repo   *Repository
	logger *slog.Logger
	now    time.Time
}

func (s syncer) entry(ctx context.Context, e *packEntry, byID map[string]*Bookmark) (syncOutcome, error) {
	want := e.snapshot()

	cur, ok := byID[want.ID.String()]
	if ok {
		have := cur.Snapshot()
		if samePackContent(have, want) {
			return syncUnchanged, nil
		}

		want.CreatedAt, want.UpdatedAt, want.Version = have.CreatedAt, s.now, have.Version+1

		b, err := Rehydrate(want)
		if err != nil {
			return 0, fmt.Errorf("pack bookmark %q: %w", e.name, err)
		}

		return syncUpdated, s.repo.Update(ctx, b, have.Version)
	}

	taken, err := s.repo.KeyTaken(ctx, want.Name, want.Frequency, want.Modulation, want.Scope, want.ID)
	if err != nil {
		return 0, err
	}

	if taken {
		s.logger.WarnContext(ctx, "pack bookmark skipped: a hub bookmark for all devices has its name, frequency and modulation",
			slog.String("name", want.Name), slog.Int64("frequency", want.Frequency), slog.String("modulation", want.Modulation))

		return syncSkipped, nil
	}

	want.CreatedAt, want.UpdatedAt = s.now, s.now

	b, err := Rehydrate(want)
	if err != nil {
		return 0, fmt.Errorf("pack bookmark %q: %w", e.name, err)
	}

	return syncInserted, s.repo.Create(ctx, b)
}

// samePackContent reports whether a stored pack row already holds an entry.
func samePackContent(a, b Snapshot) bool {
	return a.Name == b.Name && a.Frequency == b.Frequency && a.Modulation == b.Modulation && a.Underlying == b.Underlying &&
		a.Description == b.Description && a.Scannable == b.Scannable && slices.Equal(a.Tags, b.Tags) &&
		a.Scope.Kind() == ScopeAll
}
