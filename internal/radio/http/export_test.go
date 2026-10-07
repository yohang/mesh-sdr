package http

import "time"

// SetNow replaces the clock of the preset switch limit.
func (s *Streams) SetNow(now func() time.Time) { s.now = now }
