package domain_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/shared/domain"
)

func TestNewUUIDv7Layout(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 34, 56, 789_000_000, time.UTC)

	for range 100 {
		u, err := domain.NewUUIDv7(now)
		if err != nil {
			t.Fatal(err)
		}

		b := u.Bytes()
		if u.Version() != 7 || b[6]>>4 != 7 {
			t.Fatalf("version = %d", u.Version())
		}

		if b[8]>>6 != 0b10 {
			t.Fatalf("variant bits = %02b", b[8]>>6)
		}

		if !u.Time().Equal(now) {
			t.Fatalf("Time() = %v, want %v", u.Time(), now)
		}

		if s := u.String(); len(s) != 36 || s[14] != '7' || !strings.ContainsRune("89ab", rune(s[19])) {
			t.Fatalf("String() = %s", s)
		}
	}
}

func TestNewUUIDv7Range(t *testing.T) {
	if _, err := domain.NewUUIDv7(time.UnixMilli(-1)); err == nil {
		t.Error("time before the epoch accepted")
	}

	if _, err := domain.NewUUIDv7(time.UnixMilli(1 << 48)); err == nil {
		t.Error("time beyond 48 bits accepted")
	}
}

func TestParseUUID(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"0190f3a0-1c2d-7e4f-8a9b-0c1d2e3f4a5b", "0190f3a0-1c2d-7e4f-8a9b-0c1d2e3f4a5b", true},
		{"0190F3A0-1C2D-7E4F-8A9B-0C1D2E3F4A5B", "0190f3a0-1c2d-7e4f-8a9b-0c1d2e3f4a5b", true},
		{"00000000-0000-0000-0000-000000000000", "00000000-0000-0000-0000-000000000000", true},
		{"0190f3a01c2d7e4f8a9b0c1d2e3f4a5b", "", false},
		{"0190f3a0-1c2d-7e4f-8a9b-0c1d2e3f4a5", "", false},
		{"0190f3a0-1c2d-7e4f-8a9b_0c1d2e3f4a5b", "", false},
		{"g190f3a0-1c2d-7e4f-8a9b-0c1d2e3f4a5b", "", false},
		{"{0190f3a0-1c2d-7e4f-8a9b-0c1d2e3f4a5b}", "", false},
		{"", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			u, err := domain.ParseUUID(tt.in)
			if !tt.ok {
				if !errors.Is(err, domain.ErrInvalidUUID) {
					t.Fatalf("err = %v, want ErrInvalidUUID", err)
				}

				return
			}

			if err != nil || u.String() != tt.want {
				t.Fatalf("got %s, %v", u, err)
			}

			back, err := domain.UUIDFromBytes(u.Bytes())
			if err != nil || back != u {
				t.Fatalf("bytes round trip = %s, %v", back, err)
			}
		})
	}

	if !domain.MustParseUUID("00000000-0000-0000-0000-000000000000").IsZero() || (domain.UUID{}).String() != "00000000-0000-0000-0000-000000000000" {
		t.Error("nil UUID")
	}

	if _, err := domain.UUIDFromBytes(make([]byte, 15)); !errors.Is(err, domain.ErrInvalidUUID) {
		t.Errorf("15 bytes: err = %v", err)
	}
}

func TestUUIDBytesIsACopy(t *testing.T) {
	u := domain.MustParseUUID("0190f3a0-1c2d-7e4f-8a9b-0c1d2e3f4a5b")
	b := u.Bytes()
	b[0] = 0xff

	if u.String() != "0190f3a0-1c2d-7e4f-8a9b-0c1d2e3f4a5b" {
		t.Fatal("Bytes() exposes the internal array")
	}
}

func TestUUIDv7GeneratorIsMonotonic(t *testing.T) {
	g := domain.NewUUIDv7Generator()
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	clock := []time.Time{start}
	// Many ids in one millisecond (forces counter overflow), then the clock
	// goes backwards, then forward again.
	for range 5000 {
		clock = append(clock, start)
	}

	clock = append(clock, start.Add(-time.Second), start.Add(-time.Second), start.Add(time.Second))

	var prev domain.UUID

	for i, now := range clock {
		u, err := g.New(now)
		if err != nil {
			t.Fatal(err)
		}

		if u.Version() != 7 {
			t.Fatalf("version = %d", u.Version())
		}

		if i > 0 && u.Compare(prev) <= 0 {
			t.Fatalf("id %d (%s) not after %s", i, u, prev)
		}

		prev = u
	}

	if !prev.Time().Equal(start.Add(time.Second)) {
		t.Errorf("last Time() = %v", prev.Time())
	}
}

func TestUUIDv7GeneratorConcurrent(t *testing.T) {
	g := domain.NewUUIDv7Generator()
	now := time.Now()

	const workers, perWorker = 8, 500

	var (
		mu   sync.Mutex
		seen = map[domain.UUID]bool{}
		wg   sync.WaitGroup
	)

	for range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range perWorker {
				u, err := g.New(now)
				if err != nil {
					t.Error(err)

					return
				}

				mu.Lock()
				seen[u] = true
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	if len(seen) != workers*perWorker {
		t.Fatalf("%d unique ids, want %d", len(seen), workers*perWorker)
	}
}
