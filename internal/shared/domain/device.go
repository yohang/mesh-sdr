package domain

import (
	"regexp"
	"strconv"
)

// ErrInvalidDeviceID rejects a device slug that does not match DeviceIDPattern.
var ErrInvalidDeviceID = NewError(KindInvalid, "invalid_device_id", "device id must match "+DeviceIDPattern)

// DeviceIDPattern is the syntax of a device slug (TECHNICAL_SPEC §7.1 "Slugs").
const DeviceIDPattern = `^[a-z0-9][a-z0-9_-]{0,62}$`

var deviceIDPattern = regexp.MustCompile(DeviceIDPattern)

// DeviceID is the hub-wide unique device slug, shared by the hub registry,
// the node runtime, schedules and device-scoped role grants.
type DeviceID struct{ value string }

// NewDeviceID validates s.
func NewDeviceID(s string) (DeviceID, error) {
	if !deviceIDPattern.MatchString(s) {
		return DeviceID{}, ErrInvalidDeviceID.WithDetail("invalid device id " + strconv.Quote(s))
	}

	return DeviceID{value: s}, nil
}

// MustDeviceID is NewDeviceID that panics. Tests and constants only.
func MustDeviceID(s string) DeviceID {
	id, err := NewDeviceID(s)
	if err != nil {
		panic(err)
	}

	return id
}

// String returns the slug.
func (id DeviceID) String() string { return id.value }

// IsZero reports whether id is the zero value (no device).
func (id DeviceID) IsZero() bool { return id.value == "" }
