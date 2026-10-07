package connector

import (
	"strings"
	"testing"
)

func TestInstanceIDFitsTheWorkdirPattern(t *testing.T) {
	long := strings.Repeat("a", 63)
	other := strings.Repeat("a", 62) + "b"

	for _, prefix := range []string{"dev", "probe"} {
		a, b := instanceID(prefix, long), instanceID(prefix, other)

		if len(a) > maxInstanceID || len(b) > maxInstanceID || a == b || !strings.HasPrefix(a, prefix+"-aaa") {
			t.Fatalf("%q %q", a, b)
		}
	}

	if got := instanceID("dev", "rtl"); got != "dev-rtl" {
		t.Fatal(got)
	}
}
