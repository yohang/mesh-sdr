// Package domain holds the hub events model (TECHNICAL_SPEC §6.6, ADR
// 0016, ADR 0018): the topics a client subscribes to on /api/ws, who
// receives an event, and the subscription errors.
package domain

import (
	"regexp"
	"strings"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Errors of the events domain. Codes are part of the public API.
var (
	ErrInvalidTopic    = shared.NewError(shared.KindInvalid, "invalid_topic", "unknown or malformed topic")
	ErrTopicForbidden  = shared.NewError(shared.KindForbidden, "topic_forbidden", "this topic is not allowed")
	ErrUnauthenticated = shared.NewError(shared.KindUnauthenticated, "unauthenticated", "the session ended")
	ErrTooManyTopics   = shared.NewError(shared.KindInvalid, "too_many_topics", "too many topics on one connection")
	// ErrTooManyConnections refuses a socket beyond the caps of a session
	// or a client address.
	ErrTooManyConnections = shared.NewError(shared.KindRateLimited, "too_many_connections", "too many open event connections")
	// ErrUpgradeRate refuses an upgrade beyond the upgrade rate of a client
	// address (§5.12).
	ErrUpgradeRate = shared.NewError(shared.KindRateLimited, "rate_limited", "too many connection attempts")
	// ErrHubFull refuses a socket beyond the hub-wide cap.
	ErrHubFull = shared.NewError(shared.KindUnavailable, "capacity_exceeded", "the hub serves too many event connections")
)

// MaxTopics bounds the topics of one connection (ADR 0016 decision 14).
const MaxTopics = 32

// Kind is the family of a topic (§6.6).
type Kind string

// Topic kinds (§6.6 sub).
const (
	KindPresence         Kind = "presence"
	KindMap              Kind = "map"
	KindNodes            Kind = "nodes"
	KindDevices          Kind = "devices"
	KindNotifications    Kind = "notifications"
	KindAdminConnections Kind = "admin.connections"
	KindDecodes          Kind = "decodes"
	KindDiagnostics      Kind = "diagnostics"
)

var (
	global = map[Kind]bool{
		KindPresence: true, KindMap: true, KindNodes: true, KindDevices: true,
		KindNotifications: true, KindAdminConnections: true,
	}
	perDevice = map[Kind]bool{KindDecodes: true, KindDiagnostics: true}

	// identifier is the §6.1 identifier charset.
	identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// Topic is a subscription key: a global topic ("nodes") or a per-device one
// ("decodes:device=<id>"). An existing Topic is always valid.
type Topic struct {
	kind   Kind
	device string
}

// ParseTopic validates a topic as written on the wire.
func ParseTopic(s string) (Topic, error) {
	name, param, scoped := strings.Cut(s, ":")
	k := Kind(name)

	switch {
	case !scoped && global[k]:
		return Topic{kind: k}, nil
	case scoped && perDevice[k]:
		id, ok := strings.CutPrefix(param, "device=")
		if !ok || !identifier.MatchString(id) {
			return Topic{}, ErrInvalidTopic.WithDetail("expected " + name + ":device=<id>")
		}

		return Topic{kind: k, device: id}, nil
	default:
		return Topic{}, ErrInvalidTopic
	}
}

// MustTopic panics on an invalid topic. Tests and constants only.
func MustTopic(s string) Topic {
	t, err := ParseTopic(s)
	if err != nil {
		panic(err)
	}

	return t
}

// Kind returns the topic family.
func (t Topic) Kind() Kind { return t.kind }

// Device returns the device id of a per-device topic ("" otherwise).
func (t Topic) Device() string { return t.device }

// IsZero reports whether t is the zero value.
func (t Topic) IsZero() bool { return t.kind == "" }

// IsAdmin reports whether the topic is reserved to admins (admin.*).
func (t Topic) IsAdmin() bool { return strings.HasPrefix(string(t.kind), "admin.") }

// String returns the wire form.
func (t Topic) String() string {
	if t.device == "" {
		return string(t.kind)
	}

	return string(t.kind) + ":device=" + t.device
}

// Viewer is the subscriber of a connection, as audience predicates see it.
type Viewer struct {
	// UserID is the signed-in user ("" for an anonymous visitor).
	UserID string
	// SessionRef is the public handle of the session (token.SessionRef),
	// "" for an anonymous visitor.
	SessionRef string
}

// Anonymous reports whether the viewer is not signed in.
func (v Viewer) Anonymous() bool { return v.UserID == "" }

// Audience decides whether a viewer may receive an event whose existence is
// private (ADR 0016 decision 2); nil means every subscriber of the topic.
type Audience func(Viewer) bool
