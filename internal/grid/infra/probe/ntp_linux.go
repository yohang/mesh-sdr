//go:build linux

package probe

import "golang.org/x/sys/unix"

// ntpSynced reads the kernel clock state: TIME_ERROR or STA_UNSYNC mean
// the clock is not synchronised.
func ntpSynced() bool {
	var tx unix.Timex

	state, err := unix.Adjtimex(&tx)
	if err != nil {
		return true // unknown: do not raise a false alarm
	}

	return state != unix.TIME_ERROR && tx.Status&unix.STA_UNSYNC == 0
}
