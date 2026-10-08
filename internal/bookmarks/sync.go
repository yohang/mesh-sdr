package bookmarks

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

// SyncResult counts what a pack sync did.
type SyncResult struct {
	Inserted, Updated, Unchanged, Deleted int
	// Skipped counts the pack bookmarks not stored because a hub bookmark
	// for all devices has the same name, frequency and modulation (hub
	// rows win).
	Skipped int
}

// Sync stores the embedded packs as the builtin rows (BMK-002): it inserts
// the new pack bookmarks, updates the changed ones, deletes the builtin
// rows no pack holds any more, in one transaction. It is idempotent: a
// second run changes nothing. Pack ids are UUIDv5 of the unique key, so
// they are stable. Hub rows (origin db) are never touched.
func (m *Module) Sync(ctx context.Context) (SyncResult, error) {
	var res SyncResult

	now := m.now().UTC().Truncate(time.Millisecond)

	err := m.d.DB.WithinTx(ctx, func(ctx context.Context) error {
		res = SyncResult{}

		current, err := m.repo.ByOrigin(ctx, OriginBuiltin)
		if err != nil {
			return err
		}

		byID := make(map[string]*Bookmark, len(current))
		for _, b := range current {
			byID[b.ID().String()] = b
		}

		for _, e := range m.packs.entries {
			n, err := m.syncEntry(ctx, e, byID, now)
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
			if err := m.repo.Delete(ctx, b.ID(), OriginBuiltin); err != nil {
				return err
			}

			res.Deleted++
		}

		return nil
	})
	if err != nil {
		return SyncResult{}, fmt.Errorf("sync bookmark packs: %w", err)
	}

	m.d.Logger.InfoContext(ctx, "bookmark packs synced",
		slog.Int("packs", len(m.packs.files)), slog.Int("pack_bookmarks", m.packs.Len()),
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

func (m *Module) syncEntry(ctx context.Context, e *packEntry, byID map[string]*Bookmark, now time.Time) (syncOutcome, error) {
	want := e.snapshot()

	cur, ok := byID[want.ID.String()]
	if ok {
		have := cur.Snapshot()
		if samePackContent(have, want) {
			return syncUnchanged, nil
		}

		want.CreatedAt, want.UpdatedAt, want.Version = have.CreatedAt, now, have.Version+1

		b, err := Rehydrate(want)
		if err != nil {
			return 0, fmt.Errorf("pack bookmark %q: %w", e.name, err)
		}

		return syncUpdated, m.repo.Update(ctx, b, have.Version)
	}

	taken, err := m.repo.KeyTaken(ctx, want.Name, want.Frequency, want.Modulation, want.Scope, want.ID)
	if err != nil {
		return 0, err
	}

	if taken {
		m.d.Logger.WarnContext(ctx, "pack bookmark skipped: a hub bookmark for all devices has its name, frequency and modulation",
			slog.String("name", want.Name), slog.Int64("frequency", want.Frequency), slog.String("modulation", want.Modulation))

		return syncSkipped, nil
	}

	want.CreatedAt, want.UpdatedAt = now, now

	b, err := Rehydrate(want)
	if err != nil {
		return 0, fmt.Errorf("pack bookmark %q: %w", e.name, err)
	}

	return syncInserted, m.repo.Create(ctx, b)
}

// samePackContent reports whether a stored pack row already holds an entry.
func samePackContent(a, b Snapshot) bool {
	return a.Name == b.Name && a.Frequency == b.Frequency && a.Modulation == b.Modulation && a.Underlying == b.Underlying &&
		a.Description == b.Description && a.Scannable == b.Scannable && slices.Equal(a.Tags, b.Tags) &&
		a.Scope.Kind() == ScopeAll
}
