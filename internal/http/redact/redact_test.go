package redact_test

import (
	"testing"

	"github.com/yohang/mesh-sdr/internal/http/redact"
)

func TestPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/setup/s3cr3t", "/setup/{token}"},
		{"/setup/s3cr3t/x", "/setup/{token}"},
		{"//setup/s3cr3t", "//setup/{token}"},
		{"/SETUP/s3cr3t", "/SETUP/{token}"},
		{"/a/setup/s3cr3t", "/a/setup/{token}"},
		{"/invite/s3cr3t", "/invite/{token}"},
		{"/password/reset/s3cr3t", "/password/reset/{token}"},
		{"/account/email/verify/s3cr3t", "/account/email/verify/{token}"},
		{"/api/v1/auth/setup/s3cr3t", "/api/v1/auth/setup/{token}"},
		{"/setup", "/setup"},
		{"/setup/", "/setup/"},
		{"/password/reset", "/password/reset"},
		{"/receiver", "/receiver"},
	}

	for _, tt := range tests {
		if got := redact.Path(tt.in); got != tt.want {
			t.Errorf("Path(%q) = %q, want %q", tt.in, got, tt.want)
		}

		if redact.HasToken(tt.in) != (tt.in != tt.want) {
			t.Errorf("HasToken(%q) wrong", tt.in)
		}
	}
}
