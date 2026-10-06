//go:build !unix

package keyring

// lockDir is a no-op where advisory file locks are not available: run
// `meshsdr hub keys` while the hub is stopped there.
func lockDir(string) (func(), error) { return func() {}, nil }
