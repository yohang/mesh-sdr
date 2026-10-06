package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
)

func TestNewNodeID(t *testing.T) {
	tests := []struct {
		in string
		ok bool
	}{
		{"attic", true},
		{"node-1", true},
		{"0a", true},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
		{"a", false},
		{"-attic", false},
		{"Attic", false},
		{"node_1", false},
		{"node.1", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			id, err := domain.NewNodeID(tt.in)
			if tt.ok {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if id.String() != tt.in {
					t.Fatalf("String() = %q", id.String())
				}
				return
			}
			if !errors.Is(err, domain.ErrInvalidNodeID) {
				t.Fatalf("err = %v, want ErrInvalidNodeID", err)
			}
		})
	}
}
