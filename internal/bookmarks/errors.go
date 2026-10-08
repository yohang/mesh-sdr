package bookmarks

import shared "github.com/yohang/mesh-sdr/internal/shared/domain"

// Domain errors (stable codes).
var (
	ErrBookmarkNotFound = shared.NewError(shared.KindNotFound, "bookmark_not_found", "bookmark not found")
	ErrInvalidBookmark  = shared.NewError(shared.KindInvalid, "invalid_bookmark", "invalid bookmark")
	// ErrReadOnly refuses to change a pack bookmark (BMK-002: pack rows are
	// read-only).
	ErrReadOnly = shared.NewError(shared.KindConflict, "bookmark_read_only", "pack bookmarks are read-only")
	// ErrDuplicate is a second bookmark with the same name, frequency,
	// modulation and scope (TECHNICAL_SPEC §7.1 unique key, per scope).
	ErrDuplicate       = shared.NewError(shared.KindConflict, "bookmark_duplicate", "a bookmark with this name, frequency and modulation exists for this scope")
	ErrVersionConflict = shared.NewError(shared.KindConflict, "version_conflict", "the bookmark was changed meanwhile")
	// ErrDeviceNotFound is a device that does not exist, is disabled, or
	// that the caller may not listen to: the three answer the same.
	ErrDeviceNotFound = shared.NewError(shared.KindNotFound, "device_not_found", "no such device to listen to")
	// ErrInvalidRange is a frequency range with from above to or a negative
	// bound.
	ErrInvalidRange = shared.NewError(shared.KindInvalid, "invalid_range", "from must be a frequency in Hz not above to")
)
