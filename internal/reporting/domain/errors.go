package domain

import shared "github.com/yohang/mesh-sdr/internal/shared/domain"

// ErrInvalidEntry is an outbox entry with an unknown network or status, or
// a payload that is not JSON.
var ErrInvalidEntry = shared.NewError(shared.KindInvalid, "invalid_outbox_entry", "invalid outbox entry")
