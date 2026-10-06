package control

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// The hub stamps revocations once: a later broadcast of the same entry keeps
// the first time, and entries older than RevocationMemory are forgotten.
func TestRevocationMemory(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	m := NewManager(HubOptions{Now: func() time.Time { return now }, Logger: slog.New(slog.DiscardHandler)})
	ctx := context.Background()

	first := now.Add(-time.Minute)
	m.BroadcastRevocations(ctx, first, []string{"s1"}, []string{"u1"})
	m.BroadcastRevocations(ctx, time.Time{}, []string{"s1"}, nil)

	sessions, users := m.recentRevocations()
	if len(sessions) != 1 || sessions[0].At != first.UnixMilli() || len(users) != 1 || users[0].At != first.UnixMilli() {
		t.Fatalf("sessions %v users %v", sessions, users)
	}

	now = now.Add(RevocationMemory)

	if sessions, users := m.recentRevocations(); len(sessions) != 0 || len(users) != 0 {
		t.Fatalf("not forgotten: %v %v", sessions, users)
	}
}
