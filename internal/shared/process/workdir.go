package process

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// PrepareRuntimeDir creates <runtime>/sessions (0700) and checks that the
// runtime directory and its sessions directory are real directories (not
// symlinks), owned by the current user, with no group/other access (SR-53).
func PrepareRuntimeDir(runtime string) error {
	if !filepath.IsAbs(runtime) {
		return fmt.Errorf("%w: runtime dir %q is not absolute", ErrWorkdir, runtime)
	}
	if err := os.MkdirAll(runtime, 0o700); err != nil {
		return fmt.Errorf("%w: create runtime dir: %w", ErrWorkdir, err)
	}
	sessions := filepath.Join(runtime, "sessions")
	if err := os.Mkdir(sessions, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%w: create sessions dir: %w", ErrWorkdir, err)
	}
	for _, p := range []string{runtime, sessions} {
		if err := checkPrivateDir(p); err != nil {
			return err
		}
	}
	return nil
}

func checkPrivateDir(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrWorkdir, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrWorkdir, p)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %o, want 0700", ErrWorkdir, p, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%w: %s is owned by uid %d", ErrWorkdir, p, st.Uid)
	}
	return nil
}

// SweepSessions removes leftovers of crashed runs (§8.4 rule 3). It must run
// at node start, before any instance is created.
func SweepSessions(runtime string) (int, error) {
	sessions := filepath.Join(runtime, "sessions")
	entries, err := os.ReadDir(sessions)
	if err != nil {
		return 0, fmt.Errorf("%w: sweep: %w", ErrWorkdir, err)
	}
	n := 0
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(sessions, e.Name())); err != nil {
			return n, fmt.Errorf("%w: sweep %s: %w", ErrWorkdir, e.Name(), err)
		}
		n++
	}
	return n, nil
}

// createWorkdir creates <runtime>/sessions/<id> exclusively with mode 0700.
func createWorkdir(runtime, id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("%w: invalid instance id %q", ErrWorkdir, id)
	}
	p := filepath.Join(runtime, "sessions", id)
	if err := os.Mkdir(p, 0o700); err != nil {
		return "", fmt.Errorf("%w: %w", ErrWorkdir, err)
	}
	// Mkdir is subject to the umask; make the mode explicit.
	if err := os.Chmod(p, 0o700); err != nil {
		return "", fmt.Errorf("%w: %w", ErrWorkdir, err)
	}
	return p, nil
}

var fileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// CreateFile creates a file inside a workdir with exclusive-create semantics,
// through os.Root so that symlinks and ".." cannot escape the directory
// (SR-54, §8.4 rule 4). Used for generated configs such as direwolf.conf.
func CreateFile(workdir, name string, perm fs.FileMode) (*os.File, error) {
	if !fileNamePattern.MatchString(name) {
		return nil, fmt.Errorf("%w: invalid file name %q", ErrWorkdir, name)
	}
	root, err := os.OpenRoot(workdir)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWorkdir, err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWorkdir, err)
	}
	return f, nil
}
