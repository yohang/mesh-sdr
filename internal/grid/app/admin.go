package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/yohang/mesh-sdr/internal/shared/audit"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// ErrGridDisabled is returned when an action needs the hub CA.
var ErrGridDisabled = shared.NewError(shared.KindUnavailable, "grid_disabled",
	"the hub internal CA is not configured (tls.ca_cert): run `meshsdr hub ca init`")

// Links controls the control channels (implemented by the control manager).
type Links interface {
	// Drop closes the control channel of id, after pushing revocations.
	Drop(ctx context.Context, id domain.NodeID)
	// Probe asks a connected node to re-probe its capabilities.
	Probe(ctx context.Context, id domain.NodeID) error
	// Wake reconciles the set of channels now.
	Wake()
}

// NoLinks is Links when the grid is disabled.
type NoLinks struct{}

// Drop implements Links.
func (NoLinks) Drop(context.Context, domain.NodeID) {}

// Probe implements Links.
func (NoLinks) Probe(context.Context, domain.NodeID) error { return domain.ErrNodeUnavailable }

// Wake implements Links.
func (NoLinks) Wake() {}

// Issued is a node with a freshly issued enrollment token.
type Issued struct {
	Node          *domain.Node
	Token         domain.EnrollmentToken
	CAFingerprint string
	ExpiresAt     time.Time
}

// NewNodeInput declares an admin-added node.
type NewNodeInput struct {
	ID   string
	Name string
	URL  string
}

// OnChange registers a callback run after an admin change of a node
// committed: added, updated, disabled, revoked, deleted, re-enrolled
// (composition time only).
func (s *Nodes) OnChange(f func(ctx context.Context, id domain.NodeID)) {
	s.changed = append(s.changed, f)
}

func (s *Nodes) notify(ctx context.Context, id domain.NodeID) {
	for _, f := range s.changed {
		f(ctx, id)
	}
}

// SetLinks wires the control channels; until then NoLinks is used.
func (s *Nodes) SetLinks(l Links) { s.links = l }

func (s *Nodes) linksOrNone() Links {
	if s.links == nil {
		return NoLinks{}
	}

	return s.links
}

// Add declares a node (GRID-005) and issues its enrollment token.
func (s *Nodes) Add(ctx context.Context, actor audit.Actor, in NewNodeInput) (Issued, error) {
	fp, err := s.ca.Fingerprint()
	if err != nil {
		return Issued{}, ErrGridDisabled
	}

	id, err := domain.NewNodeID(in.ID)
	if err != nil {
		return Issued{}, err
	}

	nameStr := in.Name
	if nameStr == "" {
		nameStr = in.ID
	}

	name, err := domain.NewNodeName(nameStr)
	if err != nil {
		return Issued{}, err
	}

	u, err := domain.NewNodeURL(in.URL)
	if err != nil {
		return Issued{}, err
	}

	tok, err := domain.NewEnrollmentToken()
	if err != nil {
		return Issued{}, err
	}

	now := s.now()
	exp := now.Add(s.timings.EnrollmentTTL)
	n := domain.NewNode(id, name, u, now)
	n.IssueEnrollmentKey(tok.Key(), exp, now)

	if err := s.repo.Create(ctx, n); err != nil {
		return Issued{}, err
	}

	s.audit.Record(ctx, audit.Record{Actor: actor, Action: "node.add", TargetType: "node", TargetID: id.String(), Result: audit.ResultOK})
	s.logger.InfoContext(ctx, "node added", slog.String("node_id", id.String()), slog.String("url", u.String()))
	s.notify(ctx, id)

	return Issued{Node: n, Token: tok, CAFingerprint: fp, ExpiresAt: n.Snapshot().KeyExpiresAt}, nil
}

// List returns every node.
func (s *Nodes) List(ctx context.Context) ([]*domain.Node, error) { return s.repo.List(ctx) }

// Get returns one node.
func (s *Nodes) Get(ctx context.Context, id string) (*domain.Node, error) {
	nid, err := domain.NewNodeID(id)
	if err != nil {
		return nil, domain.ErrNodeNotFound
	}

	return s.repo.Get(ctx, nid)
}

// Update applies an admin change at expectedVersion.
func (s *Nodes) Update(ctx context.Context, actor audit.Actor, id string, name, url *string, disabled *bool, expectedVersion int) (*domain.Node, error) {
	var p domain.NodePatch

	if name != nil {
		v, err := domain.NewNodeName(*name)
		if err != nil {
			return nil, err
		}

		p.Name = &v
	}

	if url != nil {
		v, err := domain.NewNodeURL(*url)
		if err != nil {
			return nil, err
		}

		p.URL = &v
	}

	p.Disabled = disabled

	n, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		v := n.Version()
		if err := n.Update(p, expectedVersion, s.now()); err != nil {
			return err
		}

		return s.repo.Save(ctx, n, v)
	})
	if err != nil {
		s.audit.Record(ctx, audit.Record{Actor: actor, Action: "node.update", TargetType: "node", TargetID: id, Result: audit.ResultDenied, After: map[string]string{"reason": err.Error()}})

		return nil, err
	}

	s.audit.Record(ctx, audit.Record{Actor: actor, Action: "node.update", TargetType: "node", TargetID: id, Result: audit.ResultOK})

	if n.Disabled() {
		s.linksOrNone().Drop(ctx, n.ID())
	}

	s.linksOrNone().Wake()
	s.notify(ctx, n.ID())

	return n, nil
}

// OnDelete registers a handler run inside the transaction that deletes a
// node, before its devices go with it (composition time only).
func (s *Nodes) OnDelete(h func(ctx context.Context, id domain.NodeID) error) {
	s.onDelete = append(s.onDelete, h)
}

// Delete removes an admin-managed node and revokes its certificate.
func (s *Nodes) Delete(ctx context.Context, actor audit.Actor, id string) error {
	n, err := s.Get(ctx, id)
	if err != nil {
		return err
	}

	if err := n.CheckDeletable(); err != nil {
		s.audit.Record(ctx, audit.Record{Actor: actor, Action: "node.delete", TargetType: "node", TargetID: id, Result: audit.ResultDenied, After: map[string]string{"reason": err.Error()}})

		return err
	}

	now := s.now()

	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := revoke(ctx, s.revocations, n.PendingCertificate(), n.ID(), "deleted", now); err != nil {
			return err
		}

		if c := n.Certificate(); !c.IsZero() {
			r, err := domain.NewRevokedCertificate(c, n.ID(), "deleted", now)
			if err != nil {
				return err
			}

			if err := s.revocations.Add(ctx, r); err != nil {
				return err
			}
		}

		for _, h := range s.onDelete {
			if err := h(ctx, n.ID()); err != nil {
				return err
			}
		}

		return s.repo.Delete(ctx, n.ID())
	})
	if err != nil {
		return fmt.Errorf("delete node %s: %w", id, err)
	}

	s.audit.Record(ctx, audit.Record{Actor: actor, Action: "node.delete", TargetType: "node", TargetID: id, Result: audit.ResultOK})
	s.logger.InfoContext(ctx, "node deleted", slog.String("node_id", id))
	s.linksOrNone().Drop(ctx, n.ID())
	s.notify(ctx, n.ID())

	return nil
}

// Revoke ends the trust of an enrolled node (GRID-015): its certificate
// and any renewed one pending are added to the revocation list, the node
// stays listed as revoked (re-enroll it with IssueToken), and its control
// channel is closed after the revocation list is pushed, so the node closes
// its media connections. Config-declared nodes can be revoked too: it is
// not a deletion.
func (s *Nodes) Revoke(ctx context.Context, actor audit.Actor, id string) (*domain.Node, error) {
	n, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	now := s.now()

	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		v := n.Version()
		pending := n.PendingCertificate()

		cert, err := n.Revoke(now)
		if err != nil {
			return err
		}

		if err := revoke(ctx, s.revocations, pending, n.ID(), "revoked", now); err != nil {
			return err
		}

		if err := revoke(ctx, s.revocations, cert, n.ID(), "revoked", now); err != nil {
			return err
		}

		return s.repo.Save(ctx, n, v)
	})
	if err != nil {
		s.audit.Record(ctx, audit.Record{Actor: actor, Action: "node.revoke", TargetType: "node", TargetID: id, Result: audit.ResultDenied, After: map[string]string{"reason": err.Error()}})

		return nil, fmt.Errorf("revoke node %s: %w", id, err)
	}

	s.audit.Record(ctx, audit.Record{Actor: actor, Action: "node.revoke", TargetType: "node", TargetID: id, Result: audit.ResultOK})
	s.logger.InfoContext(ctx, "node revoked", slog.String("node_id", id))
	s.linksOrNone().Drop(ctx, n.ID())
	s.notify(ctx, n.ID())

	return n, nil
}

// EnrollLocal records the certificate the hub CA issued in-process for the
// local node of the all role (GRID-003): no token exchange, the hub and the
// node share the process. The previous certificates of the node, if any,
// are revoked. A revoked node is not re-enrolled: an admin must issue a new
// enrollment token first.
func (s *Nodes) EnrollLocal(ctx context.Context, id string, cert domain.CertInfo) error {
	n, err := s.Get(ctx, id)
	if err != nil {
		return err
	}

	if n.Enrollment() == domain.EnrollmentRevoked {
		return domain.ErrNodeRevoked
	}

	tok, err := domain.NewEnrollmentToken()
	if err != nil {
		return err
	}

	now := s.now()

	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		v := n.Version()

		if err := revoke(ctx, s.revocations, n.PendingCertificate(), n.ID(), "reissued", now); err != nil {
			return err
		}

		if err := revoke(ctx, s.revocations, n.Certificate(), n.ID(), "reissued", now); err != nil {
			return err
		}

		n.IssueEnrollmentKey(tok.Key(), time.Time{}, now)

		if err := n.CompleteEnrollment(tok.Key(), n.URL(), cert, now); err != nil {
			return err
		}

		return s.repo.Save(ctx, n, v)
	})
	if err != nil {
		return fmt.Errorf("enroll local node %s: %w", id, err)
	}

	s.audit.Record(ctx, audit.Record{Actor: audit.System, Action: "node.enroll", TargetType: "node", TargetID: id, Result: audit.ResultOK,
		After: map[string]string{"via": "local", "cert_serial": cert.Serial()}})
	s.linksOrNone().Wake()
	s.notify(ctx, n.ID())

	return nil
}

// IssueToken issues a new enrollment token (re-enrollment). The current
// certificate, if any, is revoked.
func (s *Nodes) IssueToken(ctx context.Context, actor audit.Actor, id string) (Issued, error) {
	fp, err := s.ca.Fingerprint()
	if err != nil {
		return Issued{}, ErrGridDisabled
	}

	n, err := s.Get(ctx, id)
	if err != nil {
		return Issued{}, err
	}

	tok, err := domain.NewEnrollmentToken()
	if err != nil {
		return Issued{}, err
	}

	now := s.now()
	exp := now.Add(s.timings.EnrollmentTTL)

	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		v := n.Version()
		old := n.Certificate()

		// A renewed certificate sent but not confirmed is dropped too.
		if err := revoke(ctx, s.revocations, n.PendingCertificate(), n.ID(), "superseded", now); err != nil {
			return err
		}

		n.IssueEnrollmentKey(tok.Key(), exp, now)

		if !old.IsZero() {
			r, err := domain.NewRevokedCertificate(old, n.ID(), "reenrolled", now)
			if err != nil {
				return err
			}

			if err := s.revocations.Add(ctx, r); err != nil {
				return err
			}
		}

		return s.repo.Save(ctx, n, v)
	})
	if err != nil {
		return Issued{}, fmt.Errorf("issue token for node %s: %w", id, err)
	}

	s.audit.Record(ctx, audit.Record{Actor: actor, Action: "node.enrollment_token.issue", TargetType: "node", TargetID: id, Result: audit.ResultOK})
	s.linksOrNone().Drop(ctx, n.ID())
	s.notify(ctx, n.ID())

	return Issued{Node: n, Token: tok, CAFingerprint: fp, ExpiresAt: n.Snapshot().KeyExpiresAt}, nil
}

// revoke adds cert, when set, to the revocation list.
func revoke(ctx context.Context, repo domain.RevocationRepository, cert domain.CertInfo, id domain.NodeID, reason string, now time.Time) error {
	if cert.IsZero() {
		return nil
	}

	r, err := domain.NewRevokedCertificate(cert, id, reason, now)
	if err != nil {
		return err
	}

	return repo.Add(ctx, r)
}
