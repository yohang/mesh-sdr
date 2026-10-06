package db_test

import (
	"errors"
	"testing"

	"github.com/yohang/mesh-sdr/internal/db"
)

func TestParseDSN(t *testing.T) {
	tests := []struct {
		in   string
		path string
		err  error
	}{
		{"sqlite:///var/lib/meshsdr/hub.db", "/var/lib/meshsdr/hub.db", nil},
		{"sqlite:/var/lib/meshsdr/hub.db", "/var/lib/meshsdr/hub.db", nil},
		{"sqlite:var/hub.db", "var/hub.db", nil},
		{"SQLITE:var/hub.db", "var/hub.db", nil},
		{"sqlite://host/x.db", "", db.ErrInvalidDSN},
		{"sqlite:", "", db.ErrInvalidDSN},
		{"sqlite:///", "", db.ErrInvalidDSN},
		{"sqlite:x.db?mode=memory", "", db.ErrInvalidDSN},
		{"/var/lib/hub.db", "", db.ErrInvalidDSN},
		{"hub.db", "", db.ErrInvalidDSN},
		{"postgres://u@h/db", "", db.ErrEngineUnsupported},
		{"mysql://h/db", "", db.ErrEngineUnsupported},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			d, err := db.ParseDSN(tt.in)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("err = %v, want %v", err, tt.err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if d.Dialect() != db.DialectSQLite || d.Path() != tt.path {
				t.Fatalf("got %s %q", d.Dialect(), d.Path())
			}
		})
	}
}
