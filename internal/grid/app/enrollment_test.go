package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
)

type fakeEnroller struct {
	cert domain.CertInfo
	err  error
	got  []app.EnrollmentTarget
}

func (f *fakeEnroller) Enroll(_ context.Context, t app.EnrollmentTarget) (domain.CertInfo, error) {
	f.got = append(f.got, t)

	return f.cert, f.err
}

func TestEnrollmentAttempt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	tok, _ := domain.NewEnrollmentToken()
	n := domain.NewNode(domain.MustNodeID("attic"), domain.MustNodeName("Attic"), domain.MustNodeURL("https://x:1"), e.clock.now())
	n.IssueEnrollmentKey(tok.Key(), e.clock.now().Add(time.Hour), e.clock.now())

	expired := domain.NewNode(domain.MustNodeID("old"), domain.MustNodeName("Old"), domain.MustNodeURL("https://y:1"), e.clock.now())
	expired.IssueEnrollmentKey(tok.Key(), e.clock.now().Add(-time.Minute), e.clock.now())

	for _, x := range []*domain.Node{n, expired} {
		if err := e.nodes.Create(ctx, x); err != nil {
			t.Fatal(err)
		}
	}

	cert, _ := domain.NewCertInfo(make([]byte, 32), "0F", e.clock.now().Add(90*24*time.Hour))
	f := &fakeEnroller{err: app.ErrEnrollmentRejected}
	s := app.NewEnrollment(e.nodes, e.db, f, e.audit, e.clock.now, time.Second, discard)

	enrolled := 0
	s.Enrolled = func(context.Context, domain.NodeID) { enrolled++ }

	targets, err := s.Targets(ctx)
	if err != nil || len(targets) != 1 || targets[0].ID.String() != "attic" || targets[0].Key != tok.Key() {
		t.Fatalf("targets = %+v, %v", targets, err)
	}

	if err := s.Attempt(ctx, targets[0]); !errors.Is(err, app.ErrEnrollmentRejected) {
		t.Fatalf("rejected attempt = %v", err)
	}

	f.err, f.cert = nil, cert
	if err := s.Attempt(ctx, targets[0]); err != nil {
		t.Fatal(err)
	}

	got, _ := e.nodes.Get(ctx, n.ID())
	if got.Enrollment() != domain.EnrollmentEnrolled || got.Certificate() != cert || got.HasEnrollmentKey() || enrolled != 1 {
		t.Errorf("node = %+v, enrolled callbacks %d", got.Snapshot(), enrolled)
	}

	// The token is single-use.
	if err := s.Attempt(ctx, targets[0]); !errors.Is(err, domain.ErrNodeNotPending) {
		t.Errorf("replay = %v", err)
	}

	want := []string{"node.enroll:attic", "node.enroll:attic", "node.enroll:attic"}
	if got := e.audit.actions(); len(got) != len(want) {
		t.Errorf("audit = %v", got)
	}

	if e.audit.records[0].Result != app.ResultDenied || e.audit.records[1].Result != app.ResultOK || e.audit.records[2].Result != app.ResultError {
		t.Errorf("audit results = %+v", e.audit.records)
	}
}

// An in-flight exchange whose token was rotated meanwhile must not enroll
// the node.
func TestEnrollmentAbortsWhenTokenRotated(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	tok, _ := domain.NewEnrollmentToken()
	n := domain.NewNode(domain.MustNodeID("attic"), domain.MustNodeName("Attic"), domain.MustNodeURL("https://x:1"), e.clock.now())
	n.IssueEnrollmentKey(tok.Key(), e.clock.now().Add(time.Hour), e.clock.now())

	if err := e.nodes.Create(ctx, n); err != nil {
		t.Fatal(err)
	}

	cert, _ := domain.NewCertInfo(make([]byte, 32), "0F", e.clock.now().Add(90*24*time.Hour))
	rotated, _ := domain.NewEnrollmentToken()

	// The admin rotates the token while the exchange runs.
	f := &rotatingEnroller{cert: cert, rotate: func() {
		got, _ := e.nodes.Get(ctx, n.ID())
		v := got.Version()
		got.IssueEnrollmentKey(rotated.Key(), e.clock.now().Add(time.Hour), e.clock.now())

		if err := e.nodes.Save(ctx, got, v); err != nil {
			t.Fatal(err)
		}
	}}
	s := app.NewEnrollment(e.nodes, e.db, f, e.audit, e.clock.now, time.Second, discard)

	targets, _ := s.Targets(ctx)
	if err := s.Attempt(ctx, targets[0]); !errors.Is(err, domain.ErrEnrollmentSuperseded) {
		t.Fatalf("attempt = %v, want enrollment_superseded", err)
	}

	if got, _ := e.nodes.Get(ctx, n.ID()); got.Enrollment() != domain.EnrollmentPending {
		t.Errorf("node enrolled with a rotated token: %+v", got.Snapshot())
	}
}

type rotatingEnroller struct {
	cert   domain.CertInfo
	rotate func()
}

func (r *rotatingEnroller) Enroll(context.Context, app.EnrollmentTarget) (domain.CertInfo, error) {
	r.rotate()

	return r.cert, nil
}
