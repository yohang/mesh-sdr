package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/yohang/mesh-sdr/internal/shared/audit"

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
	audit       auditor
	now         Clock
	timings     Timings
	ca          CAInfo
	links       Links
	logger      *slog.Logger
	onDelete    []func(ctx context.Context, id domain.NodeID) error
	changed     []func(ctx context.Context, id domain.NodeID)
}

// CAInfo exposes the hub CA fingerprint shown with enrollment tokens.
type CAInfo interface {
	// Fingerprint returns the colon-separated SHA-256 of the CA, or
	// domain.ErrNodeUnavailable-like error when the grid is disabled.
	Fingerprint() (string, error)
}

// NewNodes returns the service.
func NewNodes(repo domain.NodeRepository, revocations domain.RevocationRepository, tx Transactor,
	auditLog audit.Appender, ca CAInfo, timings Timings, now Clock, logger *slog.Logger,
) *Nodes {
	return &Nodes{repo: repo, revocations: revocations, tx: tx, audit: auditor{auditLog, logger}, ca: ca, timings: timings, now: now, logger: logger}
}

// SyncConfig upserts the config-declared nodes and releases the config
// nodes that are no longer declared (§7.4 "Entities", "Removal from
// config"). It runs at hub start, in one transaction.
func (s *Nodes) SyncConfig(ctx context.Context, declared []DeclaredNode) error {
	now := s.now()
	seen := map[domain.NodeID]bool{}

	var records []audit.Record

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

				records = append(records, audit.Record{Actor: audit.System, Action: "node.config.declare", TargetType: "node", TargetID: d.ID.String(), Result: audit.ResultOK})

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

				records = append(records, audit.Record{Actor: audit.System, Action: "node.config.update", TargetType: "node", TargetID: d.ID.String(), Result: audit.ResultOK})
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

				records = append(records, audit.Record{Actor: audit.System, Action: "node.config.release", TargetType: "node", TargetID: n.ID().String(), Result: audit.ResultOK})
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
