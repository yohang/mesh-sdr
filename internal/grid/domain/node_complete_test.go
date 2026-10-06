package domain_test

import (
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
)

// Completion requires the key and URL of the attempt (a rotated token or a
// moved node aborts an in-flight exchange), and does not re-check the TTL.
func TestCompleteEnrollmentChecksKeyAndURL(t *testing.T) {
	tok, _ := domain.NewEnrollmentToken()
	other, _ := domain.NewEnrollmentToken()
	url := domain.MustNodeURL("https://x:1")
	cert, _ := domain.NewCertInfo(make([]byte, 32), "01", t0.Add(time.Hour))

	n := domain.NewNode(domain.MustNodeID("attic"), domain.MustNodeName("a"), url, t0)
	n.IssueEnrollmentKey(tok.Key(), t0.Add(time.Minute), t0)

	if err := n.CompleteEnrollment(other.Key(), url, cert, t0); err == nil {
		t.Error("completion with a rotated key accepted")
	}

	if err := n.CompleteEnrollment(tok.Key(), domain.MustNodeURL("https://y:1"), cert, t0); err == nil {
		t.Error("completion for another URL accepted")
	}

	// The attempt started in time; it completes after the TTL.
	if err := n.CompleteEnrollment(tok.Key(), url, cert, t0.Add(time.Hour)); err != nil {
		t.Errorf("completion after the TTL: %v", err)
	}
}
