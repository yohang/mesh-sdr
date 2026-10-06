//go:build !linux

package probe

// ntpSynced is unknown outside Linux; report synchronised.
func ntpSynced() bool { return true }
