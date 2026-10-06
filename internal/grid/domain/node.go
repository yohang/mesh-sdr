package domain

import (
	"context"
	"slices"
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// EnrollmentState is the enrollment lifecycle of a node.
type EnrollmentState string

// Enrollment states.
const (
	EnrollmentPending  EnrollmentState = "pending"
	EnrollmentEnrolled EnrollmentState = "enrolled"
	EnrollmentRevoked  EnrollmentState = "revoked"
)

// Status is the runtime health of a node (ADR 0008 Q20).
type Status string

// Node statuses.
const (
	StatusOnline       Status = "online"
	StatusDegraded     Status = "degraded"
	StatusOffline      Status = "offline"
	StatusUnreachable  Status = "unreachable"
	StatusIncompatible Status = "incompatible"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusOnline, StatusDegraded, StatusOffline, StatusUnreachable, StatusIncompatible:
		return true
	}

	return false
}

// Origin tells where a node is declared.
type Origin string

// Origins.
const (
	OriginConfig Origin = "config"
	OriginDB     Origin = "db"
)

// Lockable node fields.
const (
	FieldName = "name"
	FieldURL  = "url"
)

// Node is a hub-side node: identity, enrollment and last known runtime
// state.
type Node struct {
	id     NodeID
	name   NodeName
	url    NodeURL
	origin Origin
	locked []string

	disabled   bool
	enrollment EnrollmentState
	key        *EnrollmentKey
	keyExpires time.Time
	enrolledAt time.Time
	cert       CertInfo

	runtime Runtime

	createdAt time.Time
	updatedAt time.Time
	version   int
}

// Runtime is the last runtime state reported by a node.
type Runtime struct {
	Status          Status
	StatusHint      string
	LastHeartbeatAt time.Time
	BootID          shared.UUID
	SoftwareVersion string
	ProtocolVersion string
	Hostname        string
	CPUCores        int
	ClockOffsetMS   *int64
}

func ms(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

// NewNode declares an admin-added node, pending enrollment.
func NewNode(id NodeID, name NodeName, url NodeURL, now time.Time) *Node {
	now = ms(now)

	return &Node{
		id: id, name: name, url: url, origin: OriginDB, locked: []string{},
		enrollment: EnrollmentPending,
		runtime:    Runtime{Status: StatusOffline},
		createdAt:  now, updatedAt: now, version: 1,
	}
}

// NewConfigNode declares a node from the hub config: name and url are
// locked; key, when set, is the config token's key (no expiry).
func NewConfigNode(id NodeID, name NodeName, url NodeURL, key *EnrollmentKey, now time.Time) *Node {
	n := NewNode(id, name, url, now)
	n.origin = OriginConfig
	n.locked = []string{FieldName, FieldURL}
	n.key = key

	return n
}

// ID returns the node id.
func (n *Node) ID() NodeID { return n.id }

// Name returns the display name.
func (n *Node) Name() NodeName { return n.name }

// URL returns the base URL the hub dials.
func (n *Node) URL() NodeURL { return n.url }

// Origin returns where the node is declared.
func (n *Node) Origin() Origin { return n.origin }

// LockedFields returns the fields set by the hub config.
func (n *Node) LockedFields() []string { return slices.Clone(n.locked) }

// Disabled reports whether the admin disabled the node.
func (n *Node) Disabled() bool { return n.disabled }

// Enrollment returns the enrollment state.
func (n *Node) Enrollment() EnrollmentState { return n.enrollment }

// HasEnrollmentKey reports whether an enrollment key is stored.
func (n *Node) HasEnrollmentKey() bool { return n.key != nil }

// StoredEnrollmentKey returns the stored key and its expiry (zero: none).
func (n *Node) StoredEnrollmentKey() (EnrollmentKey, time.Time, bool) {
	if n.key == nil {
		return EnrollmentKey{}, time.Time{}, false
	}

	return *n.key, n.keyExpires, true
}

// EnrolledAt returns the enrollment time (zero if never enrolled).
func (n *Node) EnrolledAt() time.Time { return n.enrolledAt }

// Certificate returns the current certificate identity (zero if none).
func (n *Node) Certificate() CertInfo { return n.cert }

// Runtime returns the last runtime state.
func (n *Node) Runtime() Runtime { return n.runtime }

// CreatedAt returns the creation time.
func (n *Node) CreatedAt() time.Time { return n.createdAt }

// UpdatedAt returns the last change time.
func (n *Node) UpdatedAt() time.Time { return n.updatedAt }

// Version returns the optimistic concurrency version.
func (n *Node) Version() int { return n.version }

// Active reports whether the hub keeps a control channel to the node.
func (n *Node) Active() bool { return n.enrollment == EnrollmentEnrolled && !n.disabled }

func (n *Node) touch(now time.Time) {
	n.updatedAt = ms(now)
	n.version++
}

// ApplyConfig syncs a config-declared node with the hub config. The key is
// stored only while the node is pending. It reports whether anything
// changed.
func (n *Node) ApplyConfig(name NodeName, url NodeURL, key *EnrollmentKey, now time.Time) bool {
	changed := n.origin != OriginConfig || n.name != name || n.url != url || !slices.Equal(n.locked, []string{FieldName, FieldURL})
	n.origin = OriginConfig
	n.locked = []string{FieldName, FieldURL}
	n.name = name
	n.url = url

	if n.enrollment == EnrollmentPending && key != nil && (n.key == nil || *n.key != *key || !n.keyExpires.IsZero()) {
		n.key = key
		n.keyExpires = time.Time{}
		changed = true
	}

	if changed {
		n.touch(now)
	}

	return changed
}

// ReleaseFromConfig turns a node that disappeared from the hub config into
// an admin-managed node (§7.4 "Removal from config").
func (n *Node) ReleaseFromConfig(now time.Time) bool {
	if n.origin != OriginConfig {
		return false
	}

	n.origin = OriginDB
	n.locked = []string{}
	n.touch(now)

	return true
}

// IssueEnrollmentKey (re-)opens enrollment with key until expiresAt (zero:
// no expiry). Any current certificate is forgotten; the caller revokes it.
func (n *Node) IssueEnrollmentKey(key EnrollmentKey, expiresAt, now time.Time) {
	n.enrollment = EnrollmentPending
	n.key = &key
	n.keyExpires = time.Time{}

	if !expiresAt.IsZero() {
		n.keyExpires = ms(expiresAt)
	}

	n.cert = CertInfo{}
	n.runtime.Status = StatusOffline
	n.runtime.StatusHint = ""
	n.touch(now)
}

// EnrollmentKey returns the key of a pending enrollment that has not
// expired at now.
func (n *Node) EnrollmentKey(now time.Time) (EnrollmentKey, error) {
	if n.enrollment != EnrollmentPending || n.key == nil {
		return EnrollmentKey{}, ErrNodeNotPending
	}

	if !n.keyExpires.IsZero() && !now.Before(n.keyExpires) {
		return EnrollmentKey{}, ErrEnrollmentTokenExpired
	}

	return *n.key, nil
}

// CompleteEnrollment records the issued certificate and consumes the key.
func (n *Node) CompleteEnrollment(cert CertInfo, now time.Time) error {
	if _, err := n.EnrollmentKey(now); err != nil {
		return err
	}

	if cert.IsZero() {
		return ErrInvalidCertInfo
	}

	n.enrollment = EnrollmentEnrolled
	n.key = nil
	n.keyExpires = time.Time{}
	n.enrolledAt = ms(now)
	n.cert = cert
	n.touch(now)

	return nil
}

// RenewCertificate records a renewed certificate.
func (n *Node) RenewCertificate(cert CertInfo, now time.Time) error {
	if n.enrollment != EnrollmentEnrolled {
		return ErrNodeNotEnrolled
	}

	if cert.IsZero() {
		return ErrInvalidCertInfo
	}

	n.cert = cert
	n.touch(now)

	return nil
}

// Revoke ends the node's trust. It returns the certificate to add to the
// revocation list, if any.
func (n *Node) Revoke(now time.Time) (CertInfo, error) {
	if n.enrollment == EnrollmentRevoked {
		return CertInfo{}, ErrNodeRevoked
	}

	cert := n.cert
	n.enrollment = EnrollmentRevoked
	n.key = nil
	n.keyExpires = time.Time{}
	n.cert = CertInfo{}
	n.runtime.Status = StatusOffline
	n.runtime.StatusHint = ""
	n.touch(now)

	return cert, nil
}

// NodePatch is an admin change; nil fields are unchanged.
type NodePatch struct {
	Name     *NodeName
	URL      *NodeURL
	Disabled *bool
}

// Update applies an admin change at expectedVersion. Locked fields cannot
// change.
func (n *Node) Update(p NodePatch, expectedVersion int, now time.Time) error {
	if expectedVersion != n.version {
		return ErrVersionConflict
	}

	if p.Name != nil && *p.Name != n.name && slices.Contains(n.locked, FieldName) {
		return ErrSettingLocked.WithDetail("name is set in the hub config")
	}

	if p.URL != nil && *p.URL != n.url && slices.Contains(n.locked, FieldURL) {
		return ErrSettingLocked.WithDetail("url is set in the hub config")
	}

	if p.Name != nil {
		n.name = *p.Name
	}

	if p.URL != nil {
		n.url = *p.URL
	}

	if p.Disabled != nil {
		n.disabled = *p.Disabled
	}

	n.touch(now)

	return nil
}

// CheckDeletable refuses to delete a config-declared node.
func (n *Node) CheckDeletable() error {
	if n.origin == OriginConfig {
		return ErrEntityLocked
	}

	return nil
}

// RecordWelcome records the identity reported at connect.
func (n *Node) RecordWelcome(bootID shared.UUID, softwareVersion, protocolVersion string) {
	n.runtime.BootID = bootID
	n.runtime.SoftwareVersion = softwareVersion
	n.runtime.ProtocolVersion = protocolVersion
}

// RecordHeartbeat records a heartbeat received at.
func (n *Node) RecordHeartbeat(at time.Time, clockOffsetMS int64, hostname string, cpuCores int) {
	n.runtime.LastHeartbeatAt = ms(at)
	n.runtime.ClockOffsetMS = &clockOffsetMS

	if hostname != "" {
		n.runtime.Hostname = hostname
	}

	if cpuCores > 0 {
		n.runtime.CPUCores = cpuCores
	}
}

// SetStatus changes the runtime status; it reports a transition.
func (n *Node) SetStatus(s Status, hint string) bool {
	if n.runtime.Status == s && n.runtime.StatusHint == hint {
		return false
	}

	n.runtime.Status = s
	n.runtime.StatusHint = hint

	return true
}

// NodeSnapshot is the persisted form of a Node, used by repositories only.
type NodeSnapshot struct {
	ID, Name, URL   string
	Origin          Origin
	LockedFields    []string
	Disabled        bool
	Enrollment      EnrollmentState
	EnrollmentKey   []byte
	KeyExpiresAt    time.Time
	EnrolledAt      time.Time
	CertFingerprint []byte
	CertSerial      string
	CertNotAfter    time.Time
	Runtime         Runtime
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Version         int
}

// Snapshot returns the persisted form of n.
func (n *Node) Snapshot() NodeSnapshot {
	s := NodeSnapshot{
		ID: n.id.String(), Name: n.name.String(), URL: n.url.String(),
		Origin: n.origin, LockedFields: slices.Clone(n.locked), Disabled: n.disabled,
		Enrollment: n.enrollment, KeyExpiresAt: n.keyExpires, EnrolledAt: n.enrolledAt,
		Runtime: n.runtime, CreatedAt: n.createdAt, UpdatedAt: n.updatedAt, Version: n.version,
	}

	if n.key != nil {
		s.EnrollmentKey = n.key.Bytes()
	}

	if !n.cert.IsZero() {
		fp := n.cert.Fingerprint()
		s.CertFingerprint = fp[:]
		s.CertSerial = n.cert.Serial()
		s.CertNotAfter = n.cert.NotAfter()
	}

	return s
}

// RehydrateNode rebuilds a Node from its persisted form, validating it.
func RehydrateNode(s NodeSnapshot) (*Node, error) {
	id, err := NewNodeID(s.ID)
	if err != nil {
		return nil, err
	}

	name, err := NewNodeName(s.Name)
	if err != nil {
		return nil, err
	}

	url, err := NewNodeURL(s.URL)
	if err != nil {
		return nil, err
	}

	switch s.Enrollment {
	case EnrollmentPending, EnrollmentEnrolled, EnrollmentRevoked:
	default:
		return nil, ErrInvalidCertInfo.WithDetail("unknown enrollment state " + string(s.Enrollment))
	}

	if !s.Runtime.Status.Valid() || (s.Origin != OriginConfig && s.Origin != OriginDB) || s.Version < 1 {
		return nil, ErrInvalidCertInfo.WithDetail("invalid node row " + s.ID)
	}

	n := &Node{
		id: id, name: name, url: url, origin: s.Origin, locked: slices.Clone(s.LockedFields),
		disabled: s.Disabled, enrollment: s.Enrollment, keyExpires: s.KeyExpiresAt, enrolledAt: s.EnrolledAt,
		runtime: s.Runtime, createdAt: s.CreatedAt, updatedAt: s.UpdatedAt, version: s.Version,
	}

	if n.locked == nil {
		n.locked = []string{}
	}

	if s.EnrollmentKey != nil {
		k, err := EnrollmentKeyFromBytes(s.EnrollmentKey)
		if err != nil {
			return nil, err
		}

		n.key = &k
	}

	if s.CertSerial != "" {
		c, err := NewCertInfo(s.CertFingerprint, s.CertSerial, s.CertNotAfter)
		if err != nil {
			return nil, err
		}

		n.cert = c
	}

	return n, nil
}

// NodeRepository persists nodes.
type NodeRepository interface {
	// Create inserts n; ErrNodeExists if the id is taken.
	Create(ctx context.Context, n *Node) error
	// Get loads a node; ErrNodeNotFound if absent.
	Get(ctx context.Context, id NodeID) (*Node, error)
	// List returns every node ordered by id.
	List(ctx context.Context) ([]*Node, error)
	// Save writes n if the stored version is expectedVersion, otherwise
	// ErrVersionConflict.
	Save(ctx context.Context, n *Node, expectedVersion int) error
	// SaveRuntime writes only the runtime columns of n.
	SaveRuntime(ctx context.Context, n *Node) error
	// Delete removes a node and its dependent rows; ErrNodeNotFound if absent.
	Delete(ctx context.Context, id NodeID) error
}

// RevokedCertificate is an entry of the hub revocation list.
type RevokedCertificate struct {
	serial    string
	nodeID    NodeID
	notAfter  time.Time
	revokedAt time.Time
	reason    string
}

// NewRevokedCertificate records the revocation of cert of node id.
func NewRevokedCertificate(cert CertInfo, id NodeID, reason string, now time.Time) (RevokedCertificate, error) {
	if cert.IsZero() || reason == "" {
		return RevokedCertificate{}, ErrInvalidCertInfo
	}

	return RevokedCertificate{serial: cert.Serial(), nodeID: id, notAfter: cert.NotAfter(), revokedAt: ms(now), reason: reason}, nil
}

// RehydrateRevokedCertificate rebuilds a stored entry.
func RehydrateRevokedCertificate(serial, nodeID string, notAfter, revokedAt time.Time, reason string) (RevokedCertificate, error) {
	id, err := NewNodeID(nodeID)
	if err != nil {
		return RevokedCertificate{}, err
	}

	if serial == "" {
		return RevokedCertificate{}, ErrInvalidCertInfo
	}

	return RevokedCertificate{serial: serial, nodeID: id, notAfter: notAfter, revokedAt: revokedAt, reason: reason}, nil
}

// Serial returns the revoked serial.
func (r RevokedCertificate) Serial() string { return r.serial }

// NodeID returns the node the certificate belonged to.
func (r RevokedCertificate) NodeID() NodeID { return r.nodeID }

// NotAfter returns the end of validity of the revoked certificate.
func (r RevokedCertificate) NotAfter() time.Time { return r.notAfter }

// RevokedAt returns the revocation time.
func (r RevokedCertificate) RevokedAt() time.Time { return r.revokedAt }

// Reason returns why it was revoked (revoked, deleted, reenrolled).
func (r RevokedCertificate) Reason() string { return r.reason }

// RevocationRepository persists the revocation list.
type RevocationRepository interface {
	Add(ctx context.Context, r RevokedCertificate) error
	// List returns the entries still valid at now (not after now).
	List(ctx context.Context, now time.Time) ([]RevokedCertificate, error)
}
