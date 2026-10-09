package mapfeatures

import (
	"context"
	"log/slog"
	"slices"
	"time"
)

// Ingest projects a stored decoded message of a node onto the map, inside
// the ingestion transaction of ctx (TECHNICAL_SPEC §7.3 "Transactional
// outbox" rule 1): its reports pass the report filters (MAP-013), then are
// upserted; killed objects are deleted; calls between two located stations
// become call lines, capped to map.max_calls. The changes are published by
// Flush after the commit. Only a database failure fails the batch.
func (m *Module) Ingest(ctx context.Context, d Decode) error {
	s := m.d.Settings()
	now := m.d.Now()
	p := project(d, s)

	for _, key := range p.kills {
		if err := m.remove(ctx, d.NodeID, key, ReasonDeleted); err != nil {
			return err
		}
	}

	for _, r := range p.reports {
		if err := m.report(ctx, d.NodeID, r, s, now); err != nil {
			return err
		}
	}

	for _, c := range p.calls {
		if err := m.call(ctx, d, c, s, now); err != nil {
			return err
		}
	}

	return nil
}

// report writes one report unless a filter drops it.
func (m *Module) report(ctx context.Context, node string, r report, s Settings, now time.Time) error {
	f := r.feature

	switch {
	case r.indirect && s.IgnoreIndirectReports:
		m.d.Logger.DebugContext(ctx, "indirect report dropped", slog.String("key", f.Key))

		return nil
	case !f.ExpiresAt.After(now):
		return nil
	}

	prev, found, err := m.repo.Get(ctx, f.Key)
	if err != nil {
		return err
	}

	if found {
		if s.PreferRecentReports && prev.UpdatedAt.After(f.UpdatedAt) {
			m.d.Logger.DebugContext(ctx, "older report dropped", slog.String("key", f.Key))

			return nil
		}

		// The track carries positions only from the same device: the
		// viewers of this device may not see the previous one. Those
		// viewers get the removal of the feature, which leaves their device.
		if prev.DeviceID == f.DeviceID {
			f.Geometry.Track = track(prev, f)
		} else {
			m.queue(node, Change{Remove: &Removal{Key: f.Key, Reason: ReasonDeleted, DeviceID: prev.DeviceID}})
		}
	}

	return m.upsert(ctx, node, f)
}

// track returns the track of a moving point: the previous positions, the
// last one first replaced, at most MaxTrack.
func track(prev, next Feature) []LatLon {
	if next.Geometry.Type != GeometryPoint || prev.Geometry.Type != GeometryPoint || prev.Lat == nil || prev.Lon == nil {
		return nil
	}

	out := prev.Geometry.Track
	if *prev.Lat != *next.Lat || *prev.Lon != *next.Lon {
		out = append(slices.Clone(out), LatLon{Lat: *prev.Lat, Lon: *prev.Lon})
	}

	if len(out) > MaxTrack {
		out = out[len(out)-MaxTrack:]
	}

	return out
}

// call draws the line of a call when both stations are located.
func (m *Module) call(ctx context.Context, d Decode, c call, s Settings, now time.Time) error {
	if s.MaxCalls <= 0 {
		return nil
	}

	from, ok1, err := m.located(ctx, c.from, d.DeviceID, now)
	if err != nil || !ok1 {
		return err
	}

	to, ok2, err := m.located(ctx, c.to, d.DeviceID, now)
	if err != nil || !ok2 {
		return err
	}

	details := map[string]any{"label": c.from + " → " + c.to, "mode": d.Mode, "from": c.from, "to": c.to}
	if d.FreqHz > 0 {
		details["freq_hz"] = d.FreqHz
	}

	f := Feature{
		Key: callKey(c.from, c.to), Kind: KindCall, Source: SourceDecode, DeviceID: d.DeviceID,
		Geometry: Geometry{Type: GeometryLine, From: &from, To: &to}, Details: details,
		UpdatedAt: d.At, ExpiresAt: d.At.Add(s.CallRetention),
	}

	if err := m.report(ctx, d.NodeID, report{feature: f}, s, now); err != nil {
		return err
	}

	return m.capCalls(ctx, d.NodeID, s.MaxCalls)
}

// located returns the locator of a station still on the map, reported by
// device: a line shows only what the viewers of its device may see.
func (m *Module) located(ctx context.Context, station, device string, now time.Time) (Endpoint, bool, error) {
	f, ok, err := m.repo.Get(ctx, string(KindLocator)+":"+station)
	if err != nil || !ok || f.DeviceID != device || f.Lat == nil || f.Lon == nil || (!f.ExpiresAt.IsZero() && !f.ExpiresAt.After(now)) {
		return Endpoint{}, false, err
	}

	return Endpoint{Lat: *f.Lat, Lon: *f.Lon, Callsign: station, Locator: f.Geometry.Locator}, true, nil
}

// capCalls keeps the newest max call lines (MAP-011).
func (m *Module) capCalls(ctx context.Context, node string, max int) error {
	calls, err := m.repo.OfKind(ctx, KindCall)
	if err != nil || len(calls) <= max {
		return err
	}

	for _, f := range calls[max:] {
		if err := m.remove(ctx, node, f.Key, ReasonDeleted); err != nil {
			return err
		}
	}

	return nil
}

// upsert stores a feature and queues its publication.
func (m *Module) upsert(ctx context.Context, node string, f Feature) error {
	if err := m.repo.Upsert(ctx, f); err != nil {
		return err
	}

	m.queue(node, Change{Upsert: &f})

	return nil
}

// remove deletes a feature and queues its removal.
func (m *Module) remove(ctx context.Context, node, key, reason string) error {
	f, ok, err := m.repo.Get(ctx, key)
	if err != nil || !ok {
		return err
	}

	if _, err := m.repo.Delete(ctx, key); err != nil {
		return err
	}

	m.queue(node, Change{Remove: &Removal{Key: key, Reason: reason, DeviceID: f.DeviceID}})

	return nil
}

func (m *Module) queue(node string, c Change) {
	m.mu.Lock()
	m.pending[node] = append(m.pending[node], c)
	m.mu.Unlock()
}

// Flush publishes the changes of node of the last committed batch.
func (m *Module) Flush(ctx context.Context, node string) {
	m.mu.Lock()
	changes := m.pending[node]
	delete(m.pending, node)
	m.mu.Unlock()

	if m.d.Published == nil {
		return
	}

	for _, c := range changes {
		m.d.Published(ctx, c)
	}
}

// Discard forgets the changes of node of a batch that was rolled back.
func (m *Module) Discard(node string) {
	m.mu.Lock()
	delete(m.pending, node)
	m.mu.Unlock()
}
