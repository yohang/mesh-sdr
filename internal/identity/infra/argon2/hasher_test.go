package argon2_test

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/identity/infra/argon2"
)

// cheap keeps tests fast; production floors are enforced by the config.
var cheap = argon2.Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1}

func TestHashAndVerify(t *testing.T) {
	ctx := context.Background()
	h := argon2.New(cheap, 2)

	hash, err := h.Hash(ctx, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}

	if !regexp.MustCompile(`^\$argon2id\$v=19\$m=64,t=1,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`).MatchString(hash.String()) {
		t.Errorf("PHC string = %s", hash)
	}

	other, _ := h.Hash(ctx, "correct horse battery staple")
	if other == hash {
		t.Error("salt is not random")
	}

	if ok, err := h.Verify(ctx, "correct horse battery staple", hash); !ok || err != nil {
		t.Errorf("Verify(correct) = %v, %v", ok, err)
	}

	if ok, err := h.Verify(ctx, "wrong", hash); ok || err != nil {
		t.Errorf("Verify(wrong) = %v, %v", ok, err)
	}

	if h.NeedsRehash(hash) {
		t.Error("fresh hash needs a rehash")
	}
}

func TestVerifyUsesTheStoredParameters(t *testing.T) {
	ctx := context.Background()
	old := argon2.New(argon2.Params{MemoryKiB: 32, Iterations: 2, Parallelism: 1}, 1)
	cur := argon2.New(cheap, 1)

	hash, err := old.Hash(ctx, "secret password")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := cur.Verify(ctx, "secret password", hash); !ok || err != nil {
		t.Errorf("Verify with old parameters = %v, %v", ok, err)
	}

	if !cur.NeedsRehash(hash) {
		t.Error("hash with outdated parameters does not need a rehash")
	}
}

func TestMalformedHashes(t *testing.T) {
	ctx := context.Background()
	h := argon2.New(cheap, 1)

	for _, s := range []string{
		"$argon2i$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=16$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=64,t=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA$",
		"$argon2id$v=19$m=64,t=1,p=1,x=2$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=99999999,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=64,t=1,p=1$!!$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaA",
		"$2b$12$abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQ",
	} {
		hash, err := domain.NewPasswordHash(s)
		if err != nil {
			t.Fatal(err)
		}

		if ok, err := h.Verify(ctx, "x", hash); ok || !errors.Is(err, argon2.ErrMalformedHash) {
			t.Errorf("Verify(%s) = %v, %v", s, ok, err)
		}

		if !h.NeedsRehash(hash) {
			t.Errorf("NeedsRehash(%s) = false", s)
		}
	}
}

func TestConcurrencyLimitHonoursContext(t *testing.T) {
	h := argon2.New(argon2.Params{MemoryKiB: 8 * 1024, Iterations: 3, Parallelism: 1}, 1)

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		_, _ = h.Hash(context.Background(), "occupy the only slot")
	}()

	// Either the slot was free (hash succeeds) or the wait is cancelled.
	if _, err := h.Hash(ctx, "x"); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Hash = %v", err)
	}

	<-done
}
