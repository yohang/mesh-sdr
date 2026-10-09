package domain

import "testing"

func TestTruncate(t *testing.T) {
	tests := []struct {
		s    string
		n    int
		want string
	}{
		{"", 3, ""},
		{"abc", 3, "abc"},
		{"abcd", 3, "abc"},
		{"aé", 2, "a"},
		{"aé", 3, "aé"},
		{"€", 2, ""},
		{"abc", 0, ""},
	}
	for _, tt := range tests {
		if got := Truncate(tt.s, tt.n); got != tt.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tt.s, tt.n, got, tt.want)
		}
	}
}
