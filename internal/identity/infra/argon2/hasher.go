// Package argon2 hashes local passwords with Argon2id (AUTH-017, SR-03):
// per-user random salt, parameters from auth.argon2.* encoded in a PHC
// string, constant-time verification, and re-hash detection when the stored
// parameters differ from the configured ones.
package argon2

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

const (
	saltLen = 16
	keyLen  = 32
)

// ErrMalformedHash means a stored hash is not a valid Argon2id PHC string.
// Only the account that holds it is affected.
var ErrMalformedHash = errors.New("malformed argon2id hash")

// Params are the Argon2id cost parameters.
type Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
}

// Hasher hashes and verifies passwords. Concurrent computations are limited
// so that a login flood cannot exhaust memory (MemoryKiB per hash), and so
// is the queue of requests waiting for a slot.
type Hasher struct {
	p        Params
	sem      chan struct{}
	maxQueue int64
	waiting  atomic.Int64
}

// busyRetry is the Retry-After of a request refused by a full queue.
const busyRetry = time.Second

// New returns a hasher for p, computing at most maxConcurrent hashes at a
// time (at least one) with at most maxQueue requests waiting. A request
// beyond the queue fails with a domain rate-limit error.
func New(p Params, maxConcurrent, maxQueue int) *Hasher {
	return &Hasher{p: p, sem: make(chan struct{}, max(maxConcurrent, 1)), maxQueue: int64(max(maxQueue, 0))}
}

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	default:
	}

	if h.waiting.Add(1) > h.maxQueue {
		h.waiting.Add(-1)

		return domain.NewRateLimitError(busyRetry)
	}

	defer h.waiting.Add(-1)

	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for a password hashing slot: %w", ctx.Err())
	}
}

func (h *Hasher) release() { <-h.sem }

// Hash returns the PHC string of password with a fresh random salt.
func (h *Hasher) Hash(ctx context.Context, password string) (domain.PasswordHash, error) {
	salt := make([]byte, saltLen)
	_, _ = rand.Read(salt) // never fails (crypto/rand, Go >= 1.24)

	if err := h.acquire(ctx); err != nil {
		return domain.PasswordHash{}, err
	}

	key := argon2.IDKey([]byte(password), salt, h.p.Iterations, h.p.MemoryKiB, h.p.Parallelism, keyLen)

	h.release()

	return domain.NewPasswordHash(encode(h.p, salt, key))
}

// Verify reports whether password matches hash, in constant time. It returns
// ErrMalformedHash when hash is not a supported Argon2id PHC string.
func (h *Hasher) Verify(ctx context.Context, password string, hash domain.PasswordHash) (bool, error) {
	p, salt, key, err := decode(hash.String())
	if err != nil {
		return false, err
	}

	if err := h.acquire(ctx); err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB, p.Parallelism, uint32(len(key)))

	h.release()

	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

// NeedsRehash reports whether hash was made with other parameters than the
// configured ones (or is not an Argon2id hash of this format).
func (h *Hasher) NeedsRehash(hash domain.PasswordHash) bool {
	p, salt, key, err := decode(hash.String())

	return err != nil || p != h.p || len(salt) != saltLen || len(key) != keyLen
}

func encode(p Params, salt, key []byte) string {
	b64 := base64.RawStdEncoding

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// decode parses $argon2id$v=19$m=<kib>,t=<iter>,p=<par>$<salt>$<hash>.
func decode(s string) (Params, []byte, []byte, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return Params{}, nil, nil, ErrMalformedHash
	}

	var p Params

	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return Params{}, nil, nil, ErrMalformedHash
		}

		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return Params{}, nil, nil, ErrMalformedHash
		}

		switch k {
		case "m":
			p.MemoryKiB = uint32(n)
		case "t":
			p.Iterations = uint32(n)
		case "p":
			if n > 255 {
				return Params{}, nil, nil, ErrMalformedHash
			}

			p.Parallelism = uint8(n)
		default:
			return Params{}, nil, nil, ErrMalformedHash
		}
	}

	// Bounds keep a hostile stored hash from costing unbounded work.
	if p.Iterations < 1 || p.Iterations > 64 || p.Parallelism < 1 || p.MemoryKiB < 8*uint32(p.Parallelism) || p.MemoryKiB > 4<<20 {
		return Params{}, nil, nil, ErrMalformedHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return Params{}, nil, nil, ErrMalformedHash
	}

	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > 64 {
		return Params{}, nil, nil, ErrMalformedHash
	}

	return p, salt, key, nil
}
