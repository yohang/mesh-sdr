// Package keyring keeps the Ed25519 keys that sign access tokens
// (TECHNICAL_SPEC §5.8, SR-45, ADR 0011): one 0600 file per key in a 0700
// directory of the hub state volume, never in the database or the logs.
//
// A key goes through: next (published, not yet signing: nodes learn it
// before any token uses it), current (signs), retired (still published
// until the tokens it signed have expired), then it is deleted. Rotation
// happens every rotation period, or on demand (`meshsdr hub keys rotate`);
// a revoked key is announced as such and replaced at once. The hub writes
// these files: a documented exception to "the binary never writes files",
// like the node certificate.
package keyring

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
)

// PublishLead is how long a new key is published before it signs (§5.8:
// at least 60 s).
const PublishLead = 60 * time.Second

const (
	metaFile = "keyring.json"
	keyExt   = ".ed25519"
	dirMode  = 0o700
	fileMode = 0o600
)

// ErrUnknownKey means no key has that kid.
var ErrUnknownKey = errors.New("unknown key id")

// ErrInsecure means the directory or a file is readable by others.
var ErrInsecure = errors.New("insecure key file permissions")

// entry is the metadata of a key.
type entry struct {
	Kid       string    `json:"kid"`
	CreatedAt time.Time `json:"created_at"`
	// ActivatesAt is when the key starts signing.
	ActivatesAt time.Time `json:"activates_at"`
	// RetiresAt is when a newer key took over (zero while current).
	RetiresAt time.Time `json:"retires_at,omitzero"`
	Revoked   bool      `json:"revoked,omitempty"`
}

// Options configure a keyring.
type Options struct {
	Dir string
	// Rotation is the age at which a new key is introduced.
	Rotation time.Duration
	// TokenTTL bounds how long a retired key stays published (plus the
	// verification leeway).
	TokenTTL time.Duration
}

// Keyring is a directory of signing keys. It is safe for concurrent use.
type Keyring struct {
	opts Options

	mu        sync.Mutex
	entries   []entry
	keys      map[string]ed25519.PrivateKey
	listeners []func()
	published string // fingerprint of the published set, to detect changes
}

// Open opens (creating it when missing) the keyring directory and loads it.
// A directory without keys gets one that signs at once.
func Open(opts Options, now time.Time) (*Keyring, error) {
	if opts.Dir == "" {
		return nil, errors.New("keyring: no directory")
	}

	if err := os.MkdirAll(opts.Dir, dirMode); err != nil {
		return nil, fmt.Errorf("keyring: %w", err)
	}

	k := &Keyring{opts: opts}

	k.mu.Lock()
	defer k.mu.Unlock()

	if err := k.load(); err != nil {
		return nil, err
	}

	if len(k.entries) == 0 {
		if err := k.add(now, now); err != nil {
			return nil, err
		}
	}

	k.published = k.fingerprint(now)

	return k, nil
}

func checkMode(path string, fi fs.FileInfo) error {
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s is %v, want no group or other access", ErrInsecure, path, fi.Mode().Perm())
	}

	return nil
}

// load reads the metadata and the keys (the caller holds mu).
func (k *Keyring) load() error {
	fi, err := os.Stat(k.opts.Dir)
	if err != nil {
		return fmt.Errorf("keyring: %w", err)
	}

	if err := checkMode(k.opts.Dir, fi); err != nil {
		return err
	}

	b, err := os.ReadFile(filepath.Join(k.opts.Dir, metaFile))
	if errors.Is(err, fs.ErrNotExist) {
		k.entries, k.keys = nil, map[string]ed25519.PrivateKey{}

		return nil
	}

	if err != nil {
		return fmt.Errorf("keyring: %w", err)
	}

	var entries []entry
	if err := json.Unmarshal(b, &entries); err != nil {
		return fmt.Errorf("keyring: %s: %w", metaFile, err)
	}

	keys := map[string]ed25519.PrivateKey{}

	for _, e := range entries {
		key, err := k.readKey(e.Kid)
		if err != nil {
			return err
		}

		keys[e.Kid] = key
	}

	k.entries, k.keys = entries, keys

	return nil
}

func (k *Keyring) keyPath(kid string) string { return filepath.Join(k.opts.Dir, kid+keyExt) }

func (k *Keyring) readKey(kid string) (ed25519.PrivateKey, error) {
	path := k.keyPath(kid)

	f, err := os.Open(path) //nolint:gosec // path built from the keyring directory and a thumbprint
	if err != nil {
		return nil, fmt.Errorf("keyring: %w", err)
	}

	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("keyring: %w", err)
	}

	if err := checkMode(path, fi); err != nil {
		return nil, err
	}

	buf := make([]byte, 128)

	n, err := f.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("keyring: %w", err)
	}

	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(buf[:n])))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("keyring: %s: not an Ed25519 seed", path)
	}

	key := ed25519.NewKeyFromSeed(seed)

	if got := token.Thumbprint(key.Public().(ed25519.PublicKey)); got != kid {
		return nil, fmt.Errorf("keyring: %s holds key %s", path, got)
	}

	return key, nil
}

// writeFile writes atomically with mode 0600.
func writeFile(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}

	defer func() { _ = os.Remove(tmp.Name()) }()

	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()

		return err
	}

	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()

		return err
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()

		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), path)
}

func (k *Keyring) save() error {
	b, err := json.MarshalIndent(k.entries, "", "  ")
	if err != nil {
		return err
	}

	if err := writeFile(filepath.Join(k.opts.Dir, metaFile), b); err != nil {
		return fmt.Errorf("keyring: %w", err)
	}

	return nil
}

// add creates a key that signs from activates (the caller holds mu). The
// key that signed until then retires at activates.
func (k *Keyring) add(now, activates time.Time) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("keyring: %w", err)
	}

	kid := token.Thumbprint(pub)

	if err := writeFile(k.keyPath(kid), []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n")); err != nil {
		return fmt.Errorf("keyring: %w", err)
	}

	for i := range k.entries {
		if k.entries[i].RetiresAt.IsZero() && !k.entries[i].Revoked {
			k.entries[i].RetiresAt = activates
		}
	}

	k.entries = append(k.entries, entry{Kid: kid, CreatedAt: now.UTC(), ActivatesAt: activates.UTC()})
	k.keys[kid] = priv

	return k.save()
}

// signing returns the entry that signs at now (the caller holds mu).
func (k *Keyring) signing(now time.Time) (entry, bool) {
	var best entry

	found := false

	for _, e := range k.entries {
		if e.Revoked || e.ActivatesAt.After(now) || (!e.RetiresAt.IsZero() && !now.Before(e.RetiresAt)) {
			continue
		}

		if !found || e.ActivatesAt.After(best.ActivatesAt) {
			best, found = e, true
		}
	}

	return best, found
}

// Signer returns the key that signs at now and its kid.
func (k *Keyring) Signer(now time.Time) (ed25519.PrivateKey, string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	e, ok := k.signing(now)
	if !ok {
		return nil, "", errors.New("keyring: no signing key")
	}

	return k.keys[e.Kid], e.Kid, nil
}

// retention is how long a retired key stays published.
func (k *Keyring) retention() time.Duration { return k.opts.TokenTTL + token.Leeway }

// Published returns the keys nodes must accept at now (next, current and
// recently retired ones) and the revoked kids.
func (k *Keyring) Published(now time.Time) (token.JWKS, []string) {
	k.mu.Lock()
	defer k.mu.Unlock()

	return k.publishedLocked(now)
}

func (k *Keyring) publishedLocked(now time.Time) (token.JWKS, []string) {
	jwks := token.JWKS{Keys: []token.JWK{}}
	revoked := []string{}

	for _, e := range k.entries {
		switch {
		case e.Revoked:
			revoked = append(revoked, e.Kid)
		case !e.RetiresAt.IsZero() && now.After(e.RetiresAt.Add(k.retention())):
		default:
			jwks.Keys = append(jwks.Keys, token.NewJWK(k.keys[e.Kid].Public().(ed25519.PublicKey)))
		}
	}

	return jwks, revoked
}

func (k *Keyring) fingerprint(now time.Time) string {
	jwks, revoked := k.publishedLocked(now)

	var b strings.Builder

	for _, j := range jwks.Keys {
		b.WriteString(j.Kid + ",")
	}

	b.WriteString("|" + strings.Join(revoked, ","))

	return b.String()
}

// OnChange registers f, called after the published set changes (a key
// added, retired past its retention, or revoked). Grid pushes
// ctl.keys.update from it.
func (k *Keyring) OnChange(f func()) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.listeners = append(k.listeners, f)
}

// changed notifies listeners when the published set differs from the last
// one (the caller holds mu; listeners run without it).
func (k *Keyring) changed(now time.Time) func() {
	fp := k.fingerprint(now)
	if fp == k.published {
		return func() {}
	}

	k.published = fp
	listeners := slices.Clone(k.listeners)

	return func() {
		for _, f := range listeners {
			f()
		}
	}
}

// Rotate introduces a new key that signs after PublishLead and returns its
// kid.
func (k *Keyring) Rotate(now time.Time) (string, error) {
	k.mu.Lock()

	if err := k.load(); err != nil {
		k.mu.Unlock()

		return "", err
	}

	err := k.add(now, now.Add(PublishLead))
	kid := k.entries[len(k.entries)-1].Kid
	notify := k.changed(now)
	k.mu.Unlock()
	notify()

	return kid, err
}

// Revoke marks a key revoked: nodes refuse its tokens at once. When it was
// the signing key, a new key signs immediately (no lead: the old one is
// compromised).
func (k *Keyring) Revoke(kid string, now time.Time) error {
	k.mu.Lock()

	if err := k.load(); err != nil {
		k.mu.Unlock()

		return err
	}

	i := slices.IndexFunc(k.entries, func(e entry) bool { return e.Kid == kid })
	if i < 0 {
		k.mu.Unlock()

		return fmt.Errorf("%w: %s", ErrUnknownKey, kid)
	}

	k.entries[i].Revoked = true
	if k.entries[i].RetiresAt.IsZero() {
		k.entries[i].RetiresAt = now.UTC()
	}

	var err error
	if _, ok := k.signing(now); !ok {
		err = k.add(now, now)
	} else {
		err = k.save()
	}

	notify := k.changed(now)
	k.mu.Unlock()
	notify()

	return err
}

// Maintain reloads the directory (the CLI may have rotated or revoked),
// introduces a new key when the newest one is older than the rotation
// period, deletes the keys no longer published, and notifies listeners of
// any change. The hub runs it every minute.
func (k *Keyring) Maintain(now time.Time) error {
	k.mu.Lock()

	err := k.maintainLocked(now)
	notify := k.changed(now)
	k.mu.Unlock()
	notify()

	return err
}

func (k *Keyring) maintainLocked(now time.Time) error {
	if err := k.load(); err != nil {
		return err
	}

	newest := time.Time{}

	for _, e := range k.entries {
		if !e.Revoked && e.ActivatesAt.After(newest) {
			newest = e.ActivatesAt
		}
	}

	if _, ok := k.signing(now); !ok && !newest.After(now) {
		if err := k.add(now, now); err != nil {
			return err
		}
	} else if k.opts.Rotation > 0 && !now.Before(newest.Add(k.opts.Rotation)) {
		if err := k.add(now, now.Add(PublishLead)); err != nil {
			return err
		}
	}

	kept := k.entries[:0]
	removed := false

	for _, e := range k.entries {
		gone := !e.RetiresAt.IsZero() && now.After(e.RetiresAt.Add(k.retention()))
		// A revoked key stays listed (revoked_kids) until its tokens have
		// expired anyway.
		if gone {
			if err := os.Remove(k.keyPath(e.Kid)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("keyring: %w", err)
			}

			delete(k.keys, e.Kid)

			removed = true

			continue
		}

		kept = append(kept, e)
	}

	k.entries = kept

	if removed {
		return k.save()
	}

	return nil
}

// Key describes a key for the CLI.
type Key struct {
	Kid         string
	CreatedAt   time.Time
	ActivatesAt time.Time
	RetiresAt   time.Time
	Revoked     bool
	Signing     bool
}

// List describes the keys.
func (k *Keyring) List(now time.Time) ([]Key, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if err := k.load(); err != nil {
		return nil, err
	}

	cur, _ := k.signing(now)
	out := make([]Key, 0, len(k.entries))

	for _, e := range k.entries {
		out = append(out, Key{
			Kid: e.Kid, CreatedAt: e.CreatedAt, ActivatesAt: e.ActivatesAt, RetiresAt: e.RetiresAt, Revoked: e.Revoked,
			Signing: e.Kid == cur.Kid,
		})
	}

	return out, nil
}
