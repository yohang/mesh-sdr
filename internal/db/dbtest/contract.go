package dbtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/yohang/mesh-sdr/internal/db"
)

// RunAdapterContract checks that the adapter returned by factory fulfils the
// db.Adapter engine contract. Every adapter MUST pass it. The probe table
// uses portable SQL only.
func RunAdapterContract(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("ping", func(t *testing.T) {
		if err := factory(t).Ping(context.Background()); err != nil {
			t.Fatalf("Ping: %v", err)
		}
	})

	t.Run("migrations", func(t *testing.T) { testMigrations(t, factory(t)) })
	t.Run("commit", func(t *testing.T) { testCommit(t, probe(t, factory)) })
	t.Run("rollback", func(t *testing.T) { testRollback(t, probe(t, factory)) })
	t.Run("nested transaction joins", func(t *testing.T) { testNested(t, probe(t, factory)) })
	t.Run("reader is read-only", func(t *testing.T) { testReadOnly(t, probe(t, factory)) })
	t.Run("writes are serialised", func(t *testing.T) { testSerialisedWrites(t, probe(t, factory)) })
}

func testMigrations(t *testing.T, a db.Adapter) {
	ctx := context.Background()
	m := a.Migrator()

	if err := m.Check(ctx); !errors.Is(err, db.ErrMigrationsPending) {
		t.Fatalf("Check on an empty database = %v, want ErrMigrationsPending", err)
	}

	var verr *db.VersionError
	if err := m.Check(ctx); !errors.As(err, &verr) || verr.Database != 0 || verr.Binary == 0 {
		t.Fatalf("Check = %#v, want a VersionError from 0", err)
	}

	results, err := m.Up(ctx)
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("Up applied nothing")
	}

	if err := m.Check(ctx); err != nil {
		t.Fatalf("Check after Up: %v", err)
	}

	again, err := m.Up(ctx)
	if err != nil || len(again) != 0 {
		t.Fatalf("second Up = %v, %v; want no-op", again, err)
	}

	statuses, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	for _, s := range statuses {
		if !s.Applied || s.Name == "" || s.Version == 0 {
			t.Errorf("status %+v, want applied", s)
		}
	}
}

func probe(t *testing.T, factory Factory) db.Adapter {
	t.Helper()

	a := Migrated(t, factory)
	if _, err := a.Writer(context.Background()).ExecContext(context.Background(),
		"CREATE TABLE contract_probe (id INTEGER PRIMARY KEY, v TEXT NOT NULL)"); err != nil {
		t.Fatalf("create probe table: %v", err)
	}

	return a
}

func count(t *testing.T, ctx context.Context, a db.Adapter) int {
	t.Helper()

	var n int
	if err := a.Reader(ctx).QueryRowContext(ctx, "SELECT count(*) FROM contract_probe").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}

	return n
}

func insert(ctx context.Context, a db.Adapter, v string) error {
	_, err := a.Writer(ctx).ExecContext(ctx, "INSERT INTO contract_probe (v) VALUES (?)", v)

	return err
}

func testCommit(t *testing.T, a db.Adapter) {
	ctx := context.Background()

	err := a.WithinTx(ctx, func(ctx context.Context) error {
		if err := insert(ctx, a, "a"); err != nil {
			return err
		}

		if n := count(t, ctx, a); n != 1 {
			t.Errorf("read inside the transaction = %d, want 1 (read-your-writes)", n)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("WithinTx: %v", err)
	}

	if n := count(t, ctx, a); n != 1 {
		t.Fatalf("count after commit = %d, want 1", n)
	}
}

func testRollback(t *testing.T, a db.Adapter) {
	ctx := context.Background()
	boom := errors.New("boom")

	err := a.WithinTx(ctx, func(ctx context.Context) error {
		if err := insert(ctx, a, "a"); err != nil {
			return err
		}

		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithinTx = %v, want boom", err)
	}

	if n := count(t, ctx, a); n != 0 {
		t.Fatalf("count after rollback = %d, want 0", n)
	}
}

func testNested(t *testing.T, a db.Adapter) {
	ctx := context.Background()
	boom := errors.New("boom")

	err := a.WithinTx(ctx, func(ctx context.Context) error {
		if err := a.WithinTx(ctx, func(ctx context.Context) error { return insert(ctx, a, "inner") }); err != nil {
			return err
		}

		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithinTx = %v, want boom", err)
	}

	if n := count(t, ctx, a); n != 0 {
		t.Fatalf("inner write survived the outer rollback: count = %d", n)
	}
}

func testReadOnly(t *testing.T, a db.Adapter) {
	ctx := context.Background()

	if _, err := a.Reader(ctx).ExecContext(ctx, "INSERT INTO contract_probe (v) VALUES ('x')"); err == nil {
		t.Fatal("write through Reader succeeded")
	}
}

func testSerialisedWrites(t *testing.T, a db.Adapter) {
	ctx := context.Background()

	const workers, perWorker = 8, 20

	var wg sync.WaitGroup

	errs := make(chan error, workers*perWorker)

	for w := range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range perWorker {
				errs <- a.WithinTx(ctx, func(ctx context.Context) error {
					return insert(ctx, a, fmt.Sprintf("%d-%d", w, i))
				})
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
	}

	if n := count(t, ctx, a); n != workers*perWorker {
		t.Fatalf("count = %d, want %d", n, workers*perWorker)
	}
}
