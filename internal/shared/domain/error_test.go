package domain_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/yohang/mesh-sdr/internal/shared/domain"
)

var errSample = domain.NewError(domain.KindNotFound, "sample_not_found", "sample not found")

func TestErrorIs(t *testing.T) {
	other := domain.NewError(domain.KindNotFound, "other_not_found", "other")

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"same sentinel", errSample, true},
		{"refined detail", errSample.WithDetail("sample 42 not found"), true},
		{"with violations", errSample.WithViolations(domain.NewViolation("id", "required", "id is required")), true},
		{"wrapped", fmt.Errorf("load: %w", errSample.WithDetail("x")), true},
		{"other code", other, false},
		{"plain error", errors.New("sample_not_found"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errors.Is(tt.err, errSample); got != tt.want {
				t.Fatalf("errors.Is = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestErrorAccessors(t *testing.T) {
	err := errSample.WithDetail("sample 42 not found").WithViolations(domain.NewViolation("id", "invalid", "bad id"))

	var de *domain.Error
	if !errors.As(fmt.Errorf("wrap: %w", err), &de) {
		t.Fatal("errors.As failed")
	}

	if de.Kind() != domain.KindNotFound || de.Kind().String() != "not_found" {
		t.Errorf("kind = %v", de.Kind())
	}

	if de.Code() != "sample_not_found" {
		t.Errorf("code = %q", de.Code())
	}

	if de.Message() != "sample 42 not found" {
		t.Errorf("message = %q", de.Message())
	}

	if v := de.Violations(); len(v) != 1 || v[0].Path() != "id" || v[0].Code() != "invalid" || v[0].Message() != "bad id" {
		t.Errorf("violations = %+v", v)
	}

	if want := "sample_not_found: sample 42 not found; id: bad id"; de.Error() != want {
		t.Errorf("Error() = %q, want %q", de.Error(), want)
	}

	if errSample.Message() != "sample not found" {
		t.Error("WithDetail mutated the sentinel")
	}
}
