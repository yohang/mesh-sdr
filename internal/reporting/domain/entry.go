// Package domain models the reporting outbox (TECHNICAL_SPEC §7.1
// `reporting_outbox`, §7.3 "Transactional outbox", §8.6, ADR 0020): spots
// and beacons waiting for delivery to an external network, at least once,
// with back-off.
package domain

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Network is an external network (§7.1 reporting_outbox.network).
type Network string

// Networks.
const (
	NetworkPSKReporter      Network = "pskreporter"
	NetworkWSPRnet          Network = "wsprnet"
	NetworkAPRSIS           Network = "aprs_is"
	NetworkSondeHub         Network = "sondehub"
	NetworkSondeHubListener Network = "sondehub_listener"
	NetworkAISUDP           Network = "ais_udp"
	NetworkMQTT             Network = "mqtt"
)

// Networks lists every network, in a stable order.
var Networks = []Network{
	NetworkPSKReporter, NetworkWSPRnet, NetworkAPRSIS, NetworkSondeHub, NetworkSondeHubListener, NetworkAISUDP, NetworkMQTT,
}

// ParseNetwork validates a network name.
func ParseNetwork(s string) (Network, error) {
	if n := Network(s); slices.Contains(Networks, n) {
		return n, nil
	}

	return "", ErrInvalidEntry.WithDetail("unknown network " + strconv.Quote(s))
}

// Status is the delivery state of an entry.
type Status string

// Statuses (§7.1).
const (
	StatusPending  Status = "pending"
	StatusInFlight Status = "in_flight"
	StatusSent     Status = "sent"
	StatusFailed   Status = "failed"
	StatusDead     Status = "dead"
)

// Statuses lists every status.
var Statuses = []Status{StatusPending, StatusInFlight, StatusSent, StatusFailed, StatusDead}

// ParseStatus validates a stored status.
func ParseStatus(s string) (Status, error) {
	if st := Status(s); slices.Contains(Statuses, st) {
		return st, nil
	}

	return "", ErrInvalidEntry.WithDetail("unknown status " + strconv.Quote(s))
}

// Dead reasons recorded in last_error.
const (
	ReasonDisabled = "disabled"
	ReasonOverflow = "overflow"
)

// DedupKey de-duplicates the entries of a network (§7.1, 16 bytes).
type DedupKey [16]byte

// Policy is the delivery policy of a network (§7.3 rule 2).
type Policy struct {
	// MaxAttempts and MaxAge make an entry dead.
	MaxAttempts int
	MaxAge      time.Duration
	// Lease is how long a claimed entry stays reserved for its worker.
	Lease time.Duration
}

// DefaultPolicy is the §7.3 policy: 20 attempts or 24 h, 60 s leases.
var DefaultPolicy = Policy{MaxAttempts: 20, MaxAge: 24 * time.Hour, Lease: time.Minute}

// Backoff is the delay before the next attempt after attempts failures:
// min(30 s × 2^(attempts−1), 1 h).
func Backoff(attempts int) time.Duration {
	d := 30 * time.Second

	for i := 1; i < attempts && d < time.Hour; i++ {
		d *= 2
	}

	return min(d, time.Hour)
}

// Entry is one row of the outbox.
type Entry struct {
	id          int64
	network     Network
	decodedID   *int64
	payload     json.RawMessage
	dedup       DedupKey
	status      Status
	attempts    int
	nextAttempt time.Time
	leaseOwner  string
	leaseUntil  time.Time
	batch       shared.UUID
	createdAt   time.Time
	sentAt      time.Time
	lastError   string
}

// NewEntry returns a pending entry, due now. decodedID references the
// decoded message it reports, if any.
func NewEntry(network Network, payload json.RawMessage, dedup DedupKey, decodedID *int64, now time.Time) (*Entry, error) {
	if !slices.Contains(Networks, network) || !json.Valid(payload) {
		return nil, ErrInvalidEntry
	}

	now = ms(now)

	return &Entry{
		network: network, decodedID: decodedID, payload: slices.Clone(payload), dedup: dedup, status: StatusPending,
		nextAttempt: now, createdAt: now,
	}, nil
}

func ms(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

// Sent records a delivery.
func (e *Entry) Sent(now time.Time) {
	e.status, e.sentAt, e.leaseOwner, e.leaseUntil, e.lastError = StatusSent, ms(now), "", time.Time{}, ""
}

// Failed records a failed attempt: the entry is retried after the
// back-off, or is dead after the policy's attempts or age.
func (e *Entry) Failed(cause string, p Policy, now time.Time) {
	e.attempts++
	e.leaseOwner, e.leaseUntil = "", time.Time{}
	e.lastError = truncate(cause, 512)

	if e.attempts >= p.MaxAttempts || now.Sub(e.createdAt) >= p.MaxAge {
		e.status = StatusDead

		return
	}

	e.status, e.nextAttempt = StatusFailed, ms(now.Add(Backoff(e.attempts)))
}

// Kill makes the entry dead for reason (disabled network, overflow).
func (e *Entry) Kill(reason string) {
	e.status, e.leaseOwner, e.leaseUntil, e.lastError = StatusDead, "", time.Time{}, reason
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}

	return s
}

// ID returns the row id (0 before insertion).
func (e *Entry) ID() int64 { return e.id }

// Network returns the network.
func (e *Entry) Network() Network { return e.network }

// DecodedMessageID returns the reported decoded message, if any.
func (e *Entry) DecodedMessageID() *int64 { return e.decodedID }

// Payload returns the network-specific record.
func (e *Entry) Payload() json.RawMessage { return slices.Clone(e.payload) }

// DedupKey returns the de-duplication key.
func (e *Entry) DedupKey() DedupKey { return e.dedup }

// Status returns the delivery state.
func (e *Entry) Status() Status { return e.status }

// Attempts returns the failed attempts.
func (e *Entry) Attempts() int { return e.attempts }

// NextAttempt returns when the entry is due.
func (e *Entry) NextAttempt() time.Time { return e.nextAttempt }

// Lease returns the worker holding the entry and until when.
func (e *Entry) Lease() (string, time.Time) { return e.leaseOwner, e.leaseUntil }

// Batch returns the batched upload of the entry (zero when none).
func (e *Entry) Batch() shared.UUID { return e.batch }

// CreatedAt returns the enqueue time.
func (e *Entry) CreatedAt() time.Time { return e.createdAt }

// SentAt returns the delivery time.
func (e *Entry) SentAt() time.Time { return e.sentAt }

// LastError returns the last failure or dead reason.
func (e *Entry) LastError() string { return e.lastError }

// Snapshot is the persisted form of an entry.
type Snapshot struct {
	ID          int64
	Network     Network
	DecodedID   *int64
	Payload     json.RawMessage
	Dedup       DedupKey
	Status      Status
	Attempts    int
	NextAttempt time.Time
	LeaseOwner  string
	LeaseUntil  time.Time
	Batch       shared.UUID
	CreatedAt   time.Time
	SentAt      time.Time
	LastError   string
}

// Snapshot returns the persisted form.
func (e *Entry) Snapshot() Snapshot {
	return Snapshot{
		ID: e.id, Network: e.network, DecodedID: e.decodedID, Payload: slices.Clone(e.payload), Dedup: e.dedup, Status: e.status,
		Attempts: e.attempts, NextAttempt: e.nextAttempt, LeaseOwner: e.leaseOwner, LeaseUntil: e.leaseUntil, Batch: e.batch,
		CreatedAt: e.createdAt, SentAt: e.sentAt, LastError: e.lastError,
	}
}

// Rehydrate rebuilds a stored entry.
func Rehydrate(s Snapshot) (*Entry, error) {
	if !slices.Contains(Networks, s.Network) || !slices.Contains(Statuses, s.Status) || s.ID <= 0 || s.Attempts < 0 {
		return nil, ErrInvalidEntry.WithDetail("invalid stored outbox entry " + strconv.FormatInt(s.ID, 10))
	}

	return &Entry{
		id: s.ID, network: s.Network, decodedID: s.DecodedID, payload: slices.Clone(s.Payload), dedup: s.Dedup, status: s.Status,
		attempts: s.Attempts, nextAttempt: s.NextAttempt, leaseOwner: s.LeaseOwner, leaseUntil: s.LeaseUntil, batch: s.Batch,
		createdAt: s.CreatedAt, sentAt: s.SentAt, lastError: s.LastError,
	}, nil
}

// NetworkStats is the queue of one network (§6.10 GET /reporting/status).
type NetworkStats struct {
	Network Network
	Counts  map[Status]int64
	// LastSentAt is the last delivery; OldestDueAt the oldest entry
	// waiting (pending or failed). Zero when none.
	LastSentAt, OldestDueAt time.Time
}

// Repository persists the outbox.
type Repository interface {
	// Enqueue inserts a pending entry and reports whether it was new: an
	// entry with the same network and dedup key is not inserted again.
	Enqueue(ctx context.Context, e *Entry) (bool, error)
	// Claim leases up to limit entries of network due at now (pending or
	// failed, or in flight with an expired lease) to owner until until,
	// oldest first.
	Claim(ctx context.Context, network Network, owner string, now, until time.Time, limit int) ([]*Entry, error)
	// Save writes the delivery state of a claimed entry while owner still
	// holds it, and reports whether it did.
	Save(ctx context.Context, e *Entry, owner string) (bool, error)
	// KillWaiting makes dead, with reason, the waiting entries of network
	// enqueued before before.
	KillWaiting(ctx context.Context, network Network, before time.Time, reason string) (int64, error)
	// KillOverflow makes dead the oldest pending entries of network beyond
	// keep.
	KillOverflow(ctx context.Context, network Network, keep int) (int64, error)
	// Purge deletes up to limit entries of status older than before (the
	// delivery time of sent entries, the enqueue time of dead ones).
	Purge(ctx context.Context, status Status, before time.Time, limit int) (int64, error)
	// Stats returns the queue of every network.
	Stats(ctx context.Context) ([]NetworkStats, error)
}
