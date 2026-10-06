package domain_test

import (
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
)

// A config token never replaces an admin-issued key nor revives after an
// enrollment or a revocation.
func TestApplyConfigKeepsAdminKeysAndUsedTokens(t *testing.T) {
	cfgTok, _ := domain.NewEnrollmentToken()
	adminTok, _ := domain.NewEnrollmentToken()
	cfgKey := cfgTok.Key()
	name, url := domain.MustNodeName("Attic"), domain.MustNodeURL("https://x:1")

	n := domain.NewConfigNode(domain.MustNodeID("attic"), name, url, &cfgKey, t0)

	// The admin re-issues a token: a hub restart keeps it.
	n.IssueEnrollmentKey(adminTok.Key(), t0.Add(time.Hour), t0)
	n.ApplyConfig(name, url, &cfgKey, t0)

	if k, _, _ := n.StoredEnrollmentKey(); k != adminTok.Key() {
		t.Fatal("config token replaced the admin-issued key")
	}

	// After enrollment the config token is not revived.
	cert, _ := domain.NewCertInfo(make([]byte, 32), "01", t0.Add(time.Hour))
	if err := n.CompleteEnrollment(adminTok.Key(), url, cert, t0); err != nil {
		t.Fatal(err)
	}

	n.ApplyConfig(name, url, &cfgKey, t0)

	if n.HasEnrollmentKey() || n.Enrollment() != domain.EnrollmentEnrolled {
		t.Fatal("config token revived after enrollment")
	}

	// Nor after a revocation.
	if _, err := n.Revoke(t0); err != nil {
		t.Fatal(err)
	}

	n.ApplyConfig(name, url, &cfgKey, t0)

	if n.HasEnrollmentKey() {
		t.Fatal("config token revived after revocation")
	}

	// A pending config node without key gets the config key.
	fresh := domain.NewConfigNode(domain.MustNodeID("garden"), name, url, nil, t0)
	if !fresh.ApplyConfig(name, url, &cfgKey, t0) || !fresh.HasEnrollmentKey() {
		t.Fatal("config key not seeded")
	}
}
