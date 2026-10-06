package commonpw_test

import (
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/identity/infra/commonpw"
)

func TestEmbeddedList(t *testing.T) {
	l, err := commonpw.Load()
	if err != nil {
		t.Fatal(err)
	}

	if l.Len() < 30000 {
		t.Errorf("list has %d entries", l.Len())
	}

	for _, pw := range []string{"password", "PASSWORD", "12345678", "iloveyou", "Password1"} {
		if !l.Contains(pw) {
			t.Errorf("%q not in the list", pw)
		}
	}

	if l.Contains("correct horse battery staple 42") {
		t.Error("an uncommon password is in the list")
	}

	if _, err := domain.NewPassword("qwertyuiop", domain.DefaultPasswordPolicy().WithCommonPasswords(l)); err == nil {
		t.Error("common password accepted by the policy")
	}
}

func TestParse(t *testing.T) {
	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte("Zebra\nalpha\n\nalpha\n"))
	_ = zw.Close()

	l, err := commonpw.Parse(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}

	if l.Len() != 2 || !l.Contains("ZEBRA") || !l.Contains("Alpha") || l.Contains("beta") {
		t.Errorf("parsed list wrong (len %d)", l.Len())
	}

	if _, err := commonpw.Parse([]byte("not gzip")); err == nil {
		t.Error("invalid data accepted")
	}
}
