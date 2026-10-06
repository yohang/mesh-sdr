package rxv1

import (
	"fmt"
	"unicode/utf8"
)

// MessageType is the "type" of an envelope. Valid values match
// ^[a-z][a-z0-9_]*(\.[a-z0-9_]+)*$ (§6.2). The constants below are valid by
// construction; values read from the wire go through ParseMessageType.
type MessageType string

// Message types shared by every rx.v1 channel.
const (
	TypeAck            MessageType = "ack"
	TypeError          MessageType = "error"
	TypeSessionHello   MessageType = "session.hello"
	TypeSessionWelcome MessageType = "session.welcome"
)

// Node media WS, client → node (§6.4).
const (
	TypeAuthRefresh     MessageType = "auth.refresh"
	TypeTimeSync        MessageType = "time.sync"
	TypeDeviceAttach    MessageType = "device.attach"
	TypeDeviceDetach    MessageType = "device.detach"
	TypeStreamConfigure MessageType = "stream.configure"
	TypeAudioConfigure  MessageType = "audio.configure"
	TypeDemodCreate     MessageType = "demod.create"
	TypeDemodSet        MessageType = "demod.set"
	TypeDemodRemove     MessageType = "demod.remove"
	TypeDecoderSet      MessageType = "decoder.set"
	TypePresetSelect    MessageType = "preset.select"
	TypeDeviceRetune    MessageType = "device.retune"
	TypeBye             MessageType = "bye"
)

// Node media WS, node → client (§6.5).
const (
	TypeDeviceConfig      MessageType = "device.config"
	TypeDeviceConfigPatch MessageType = "device.config.patch"
	TypeDeviceState       MessageType = "device.state"
	TypeStreamOpen        MessageType = "stream.open"
	TypeStreamUpdate      MessageType = "stream.update"
	TypeStreamClose       MessageType = "stream.close"
	TypeDemodMeter        MessageType = "demod.meter"
	TypeDemodMeta         MessageType = "demod.meta"
	TypeDecode            MessageType = "decode"
	TypeDiagState         MessageType = "diag.state"
	TypeNotice            MessageType = "notice"
	TypeTimeSyncReply     MessageType = "time.sync.reply"
)

// Hub events WS (§6.6).
const (
	TypeSub               MessageType = "sub"
	TypeUnsub             MessageType = "unsub"
	TypePresenceHeartbeat MessageType = "presence.heartbeat"

	TypePresenceCount    MessageType = "presence.count"
	TypePresenceList     MessageType = "presence.list"
	TypePresenceJoined   MessageType = "presence.joined"
	TypePresenceLeft     MessageType = "presence.left"
	TypeMapSnapshot      MessageType = "map.snapshot"
	TypeMapFeatureUpsert MessageType = "map.feature.upsert"
	TypeMapFeatureRemove MessageType = "map.feature.remove"
	TypeDecodeNew        MessageType = "decode.new"
	TypeNodeStatus       MessageType = "node.status"
	TypeDeviceStatus     MessageType = "device.status"
	TypePresetChanged    MessageType = "preset.changed"
	TypeBookmarkChanged  MessageType = "bookmark.changed"
	TypeSettingsChanged  MessageType = "settings.changed"
	TypeNotification     MessageType = "notification"
	TypeFilesNew         MessageType = "files.new"
	TypeSessionRevoked   MessageType = "session.revoked"
)

// ParseMessageType validates s against the §6.2 type grammar.
func ParseMessageType(s string) (MessageType, error) {
	if !validMessageType(s) {
		return "", &Error{Code: CodeInvalidEnvelope, Path: "type", Reason: fmt.Sprintf("invalid message type %q", truncate(s))}
	}
	return MessageType(s), nil
}

// String returns the wire form of t.
func (t MessageType) String() string { return string(t) }

// validMessageType implements ^[a-z][a-z0-9_]*(\.[a-z0-9_]+)*$ without regexp.
func validMessageType(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	prevDot := false
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '.':
			if prevDot {
				return false
			}
			prevDot = true
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_':
			prevDot = false
		default:
			return false
		}
	}
	return !prevDot
}

// Catalogue is the set of message types a receiver accepts on one channel and
// direction (§6.3). A syntactically valid type outside the catalogue is
// answered with error unsupported_type; the connection stays open.
type Catalogue struct {
	name  string
	types map[MessageType]struct{}
}

func newCatalogue(name string, types ...MessageType) Catalogue {
	m := make(map[MessageType]struct{}, len(types))
	for _, t := range types {
		m[t] = struct{}{}
	}
	return Catalogue{name: name, types: m}
}

// Name identifies the catalogue (e.g. "media client→node").
func (c Catalogue) Name() string { return c.name }

// Contains reports whether t belongs to the catalogue.
func (c Catalogue) Contains(t MessageType) bool {
	_, ok := c.types[t]
	return ok
}

// Check returns an unsupported_type *Error when t is not in the catalogue.
func (c Catalogue) Check(t MessageType) error {
	if c.Contains(t) {
		return nil
	}
	return &Error{Code: CodeUnsupportedType, Path: "type", Reason: fmt.Sprintf("message type %q is not supported on this channel", truncate(string(t)))}
}

var (
	mediaClientToNode = newCatalogue("media client→node",
		TypeSessionHello, TypeAuthRefresh, TypeTimeSync, TypeDeviceAttach, TypeDeviceDetach,
		TypeStreamConfigure, TypeAudioConfigure, TypeDemodCreate, TypeDemodSet, TypeDemodRemove,
		TypeDecoderSet, TypePresetSelect, TypeDeviceRetune, TypeBye)
	mediaNodeToClient = newCatalogue("media node→client",
		TypeSessionWelcome, TypeDeviceConfig, TypeDeviceConfigPatch, TypeDeviceState,
		TypeStreamOpen, TypeStreamUpdate, TypeStreamClose, TypeDemodMeter, TypeDemodMeta,
		TypeDecode, TypeDiagState, TypeNotice, TypeTimeSyncReply, TypeAck, TypeError)
	hubClientToHub = newCatalogue("hub client→hub",
		TypeSessionHello, TypeSub, TypeUnsub, TypePresenceHeartbeat)
	hubHubToClient = newCatalogue("hub hub→client",
		TypeSessionWelcome, TypePresenceCount, TypePresenceList, TypePresenceJoined, TypePresenceLeft,
		TypeMapSnapshot, TypeMapFeatureUpsert, TypeMapFeatureRemove, TypeDecodeNew, TypeDiagState,
		TypeNodeStatus, TypeDeviceStatus, TypePresetChanged, TypeBookmarkChanged, TypeSettingsChanged,
		TypeNotification, TypeFilesNew, TypeSessionRevoked, TypeAck, TypeError)
)

// MediaClientToNode is the catalogue a node accepts on /ws (§6.4).
func MediaClientToNode() Catalogue { return mediaClientToNode }

// MediaNodeToClient is the catalogue a client accepts on the media WS (§6.5).
func MediaNodeToClient() Catalogue { return mediaNodeToClient }

// HubClientToHub is the catalogue the hub accepts on /api/ws (§6.6).
func HubClientToHub() Catalogue { return hubClientToHub }

// HubHubToClient is the catalogue a client accepts on /api/ws (§6.6).
func HubHubToClient() Catalogue { return hubHubToClient }

// truncate bounds attacker-controlled strings echoed in error reasons.
func truncate(s string) string {
	const maxLen = 64
	if len(s) <= maxLen {
		return s
	}
	cut := maxLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
