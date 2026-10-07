package jobs_test

import (
	"context"
	"testing"

	"github.com/yohang/mesh-sdr/internal/db/dbtest"
	"github.com/yohang/mesh-sdr/internal/jobs"
)

func TestTableStats(t *testing.T) {
	a := dbtest.NewSQLite(t)

	s, err := jobs.NewTableStats(a, "roles")
	if err != nil {
		t.Fatal(err)
	}

	rows, bytes, sized, err := s.Stats(context.Background())
	if err != nil || rows != 4 {
		t.Fatalf("stats = %d rows, %v", rows, err)
	}

	if sized && bytes <= 0 {
		t.Errorf("size = %d", bytes)
	}

	t.Logf("dbstat available: %v (%d bytes)", sized, bytes)

	if _, err := jobs.NewTableStats(a, "roles; DROP TABLE users"); err == nil {
		t.Error("invalid table name accepted")
	}
}
