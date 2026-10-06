package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
)

// DeclaredNode is a node declared in the hub config (nodes.<id>.*).
type DeclaredNode struct {
	ID    domain.NodeID
	Name  domain.NodeName
	URL   domain.NodeURL
	Token *domain.EnrollmentToken
}

// Nodes manages the node registry: config sync and admin use cases.
type Nodes struct {
	repo        domain.NodeRepository
	revocations domain.RevocationRepository
	tx          Transactor
	audit       Auditor
	now         Clock
	timings     Timings
	ca          CAInfo
	logger      *slog.Logger
}

// CAInfo exposes the hub CA fingerprint shown with enrollment tokens.
type CAInfo interface {
	// Fingerprint returns the colon-separated SHA-256 of the CA, or
	// domain.ErrNodeUnavailable-like error when the grid is disabled.
	Fingerprint() (string, error)
}

// NewNodes returns the service.
func NewNodes(repo domain.NodeRepository, revocations domain.RevocationRepository, tx Transactor,
	audit Auditor, ca CAInfo, timings Timings, now Clock, logger *slog.Logger,
) *Nodes {
	return &Nodes{repo: repo, revocations: revocations, tx: tx, audit: audit, ca: ca, timings: timings, now: now, logger: logger}
}

// SyncConfig upserts the config-declared nodes and releases the config
// nodes that are no longer declared (§7.4 "Entities", "Removal from
// config"). It runs at hub start, in one transaction.
func (s *Nodes) SyncConfig(ctx context.Context, declared []DeclaredNode) error {
	now := s.now()
	seen := map[domain.NodeID]bool{}

	var records []AuditRecord

	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		records = records[:0]

		for _, d := range declared {
			seen[d.ID] = true

			var key *domain.EnrollmentKey
			if d.Token != nil {
				k := d.Token.Key()
				key = &k
			}

			n, err := s.repo.Get(ctx, d.ID)
			if errors.Is(err, domain.ErrNodeNotFound) {
				if err := s.repo.Create(ctx, domain.NewConfigNode(d.ID, d.Name, d.URL, key, now)); err != nil {
					return fmt.Errorf("declare node %s: %w", d.ID, err)
				}

				records = append(records, AuditRecord{ActorKind: ActorSystem, Action: "node.config.declare", Target: d.ID.String(), Result: ResultOK})

				continue
			}

			if err != nil {
				return err
			}

			v := n.Version()
			if n.ApplyConfig(d.Name, d.URL, key, now) {
				if err := s.repo.Save(ctx, n, v); err != nil {
					return fmt.Errorf("sync node %s: %w", d.ID, err)
				}

				records = append(records, AuditRecord{ActorKind: ActorSystem, Action: "node.config.update", Target: d.ID.String(), Result: ResultOK})
			}
		}

		all, err := s.repo.List(ctx)
		if err != nil {
			return err
		}

		for _, n := range all {
			if seen[n.ID()] {
				continue
			}

			v := n.Version()
			if n.ReleaseFromConfig(now) {
				if err := s.repo.Save(ctx, n, v); err != nil {
					return fmt.Errorf("release node %s: %w", n.ID(), err)
				}

				records = append(records, AuditRecord{ActorKind: ActorSystem, Action: "node.config.release", Target: n.ID().String(), Result: ResultOK})
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("sync config nodes: %w", err)
	}

	for _, r := range records {
		s.audit.Record(ctx, r)
	}

	s.logger.InfoContext(ctx, "config nodes synced", slog.Int("declared", len(declared)), slog.Int("changes", len(records)))

	return nil
}
