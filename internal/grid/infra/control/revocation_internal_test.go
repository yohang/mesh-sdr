package control

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// The hub keeps the latest revocation of each id: a second revocation of
// the same user moves its time forward (an earlier one never moves it back),
// and entries older than RevocationMemory are forgotten.
func TestRevocationMemory(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	m := NewManager(HubOptions{Now: func() time.Time { return now }, Logger: slog.New(slog.DiscardHandler)})
	ctx := context.Background()

	first := now.Add(-time.Minute)
	m.BroadcastRevocations(ctx, first, []string{"s1"}, []string{"u1"})
	m.BroadcastRevocations(ctx, time.Time{}, []string{"s1"}, nil)
	m.BroadcastRevocations(ctx, first.Add(-time.Minute), nil, []string{"u1"})

	sessions, users := m.recentRevocations()
	if len(sessions) != 1 || sessions[0].At != now.UnixMilli() || len(users) != 1 || users[0].At != first.UnixMilli() {
		t.Fatalf("sessions %v users %v", sessions, users)
	}

	now = now.Add(RevocationMemory + time.Millisecond)

	if sessions, users := m.recentRevocations(); len(sessions) != 0 || len(users) != 0 {
		t.Fatalf("not forgotten: %v %v", sessions, users)
	}
}
