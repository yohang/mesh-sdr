package pki

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// CreateCA creates a hub CA at certPath and keyPath (0600). It never
// replaces a CA: an existing file is an error, even with a concurrent run.
func CreateCA(certPath, keyPath string, now time.Time) (*CA, error) {
	certPEM, keyPEM, err := GenerateCA("MeshSDR hub CA", now)
	if err != nil {
		return nil, err
	}

	if err := createFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}

	if err := createFile(certPath, certPEM, 0o644); err != nil {
		_ = os.Remove(keyPath)

		return nil, err
	}

	return ParseCA(certPEM, keyPEM)
}

// createFile is WriteFileExclusive, an existing file named in its error.
func createFile(path string, data []byte, perm os.FileMode) error {
	err := WriteFileExclusive(path, data, perm)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s already exists: refusing to overwrite the hub CA", path)
	}

	return err
}

// LoadKeyPair reads a PEM certificate chain and its key from files. The key
// file must not be readable or writable by group or others.
func LoadKeyPair(certFile, keyFile string) (tls.Certificate, error) {
	info, err := os.Stat(keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("key file: %w", err)
	}

	if info.Mode().Perm()&0o077 != 0 {
		return tls.Certificate{}, fmt.Errorf("key file %s has mode %04o, want 0600", keyFile, info.Mode().Perm())
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load key pair: %w", err)
	}

	return cert, nil
}

// stageFile writes data to a synced temporary file next to path and
// returns its name.
func stageFile(path string, data []byte, perm os.FileMode) (_ string, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}

	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}

	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()

	if err := f.Chmod(perm); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}

	if _, err := f.Write(data); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}

	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}

	if err := f.Close(); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}

	return f.Name(), nil
}

// syncDir makes the renames and links in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // directory of a configured path
	if err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}

	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}

	return nil
}

// File is one file of a WriteFilesAtomic batch.
type File struct {
	Path string
	Data []byte
	Perm os.FileMode
}

// WriteFileAtomic writes data to path through a temporary file in the same
// directory and a rename, so readers never see a partial file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return WriteFilesAtomic(File{Path: path, Data: data, Perm: perm})
}

// WriteFilesAtomic stages every file first, then renames them in the given
// order and syncs their directories: a failure while writing leaves every
// target untouched, and a crash between renames leaves a prefix of the
// batch in place.
func WriteFilesAtomic(files ...File) error {
	temps := make([]string, 0, len(files))

	cleanup := func() {
		for _, t := range temps {
			_ = os.Remove(t)
		}
	}

	for _, f := range files {
		t, err := stageFile(f.Path, f.Data, f.Perm)
		if err != nil {
			cleanup()

			return err
		}

		temps = append(temps, t)
	}

	dirs := map[string]bool{}

	for i, f := range files {
		if err := os.Rename(temps[i], f.Path); err != nil {
			temps = temps[i:]
			cleanup()

			return fmt.Errorf("write %s: %w", f.Path, err)
		}

		dirs[filepath.Dir(f.Path)] = true
	}

	for d := range dirs {
		if err := syncDir(d); err != nil {
			return err
		}
	}

	return nil
}

// WriteFileExclusive creates path with data, failing with fs.ErrExist when
// it already exists: the file is staged, then hard-linked into place, which
// never replaces an existing file.
func WriteFileExclusive(path string, data []byte, perm os.FileMode) error {
	t, err := stageFile(path, data, perm)
	if err != nil {
		return err
	}

	defer func() { _ = os.Remove(t) }()

	if err := os.Link(t, path); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}

	return syncDir(filepath.Dir(path))
}
