package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
)

// ReportHandler runs, inside the ingestion transaction, after a changed
// capability report was stored (the device registry mirror).
type ReportHandler func(ctx context.Context, n *domain.Node, caps ctl.Capabilities, now time.Time) error

// Capabilities stores node capability reports (§4.7, GRID-010).
type Capabilities struct {
	repo     domain.CapabilityRepository
	nodes    domain.NodeRepository
	links    Links
	logger   *slog.Logger
	onReport []ReportHandler
}

// NewCapabilities returns the service.
func NewCapabilities(repo domain.CapabilityRepository, nodes domain.NodeRepository, links Links, logger *slog.Logger) *Capabilities {
	if links == nil {
		links = NoLinks{}
	}

	return &Capabilities{repo: repo, nodes: nodes, links: links, logger: logger}
}

// OnReport registers a handler of changed reports (composition time only).
func (s *Capabilities) OnReport(h ReportHandler) { s.onReport = append(s.onReport, h) }

// Hash identifies the content of a report, seq excluded.
func Hash(c ctl.Capabilities) (string, json.RawMessage, error) {
	c.Seq = 0

	doc, err := json.Marshal(c)
	if err != nil {
		return "", nil, err
	}

	sum := sha256.Sum256(doc)

	return hex.EncodeToString(sum[:16]), doc, nil
}

// Rows derives the namespaced capability rows of a report (§7.1).
func Rows(c ctl.Capabilities) ([]domain.Capability, error) {
	var rows []domain.Capability

	seen := map[string]bool{}
	add := func(key string, available bool, status domain.CapabilityStatus, version string, detail any, errMsg string) error {
		if seen[key] {
			return nil
		}

		seen[key] = true

		raw, err := json.Marshal(detail)
		if err != nil {
			return err
		}

		row, err := domain.NewCapability(key, available, status, version, raw, errMsg)
		if err != nil {
			return err
		}

		rows = append(rows, row)

		return nil
	}

	status := func(ok bool) domain.CapabilityStatus {
		if ok {
			return domain.CapabilityOK
		}

		return domain.CapabilityMissing
	}

	for _, d := range c.SDRDrivers {
		if err := add("driver:"+d.Type, d.Available, status(d.Available), d.Version, map[string]any{}, d.Reason); err != nil {
			return nil, err
		}
	}

	for _, dec := range c.Decoders {
		allOK := true

		var reasons []string

		for _, t := range dec.Tools {
			allOK = allOK && t.OK
			if !t.OK && t.Reason != "" {
				reasons = append(reasons, t.Reason)
			}

			if err := add("tool:"+t.Name, t.OK, status(t.OK), t.Version, map[string]any{}, truncate(t.Reason, 512)); err != nil {
				return nil, err
			}
		}

		// DIAG-004: a missing capability says which tool is missing.
		reason := truncate(strings.Join(reasons, "; "), 512)

		if err := add("feature:"+dec.Cap, allOK, status(allOK), "", map[string]any{"modes": dec.Modes}, reason); err != nil {
			return nil, err
		}

		for _, m := range dec.Modes {
			if err := add("mode:"+m, allOK, status(allOK), "", map[string]any{"cap": dec.Cap}, reason); err != nil {
				return nil, err
			}
		}
	}

	for _, codec := range c.AudioCodecs {
		if err := add("codec:"+codec, true, domain.CapabilityOK, "", map[string]any{"kind": "audio"}, ""); err != nil {
			return nil, err
		}
	}

	for _, codec := range c.FFTCodecs {
		if err := add("codec:"+codec, true, domain.CapabilityOK, "", map[string]any{"kind": "fft"}, ""); err != nil {
			return nil, err
		}
	}

	if c.Codecserver.Available || c.Codecserver.AMBE {
		if err := add("codec:ambe", c.Codecserver.AMBE, status(c.Codecserver.AMBE), "", map[string]any{"codecserver": c.Codecserver.Available}, ""); err != nil {
			return nil, err
		}
	}

	return rows, nil
}

// Handler applies node.capabilities events (control ingestion).
func (s *Capabilities) Handler() EventHandler {
	return func(ctx context.Context, n *domain.Node, ev Event, now time.Time) error {
		caps, err := decodeEvent[ctl.Capabilities](ev)
		if err != nil {
			s.logger.WarnContext(ctx, "invalid node.capabilities skipped", slog.String("node_id", n.ID().String()), slog.Any("error", err))

			return nil
		}

		hash, doc, err := Hash(caps)
		if err != nil {
			return err
		}

		n.RecordPlatform(caps.Platform.Hostname, caps.Platform.CPUCores)

		if prev, err := s.repo.Get(ctx, n.ID()); err == nil && prev.Hash() == hash {
			return nil
		} else if err != nil && !errors.Is(err, domain.ErrCapabilitiesNotReported) {
			return err
		}

		rows, err := Rows(caps)
		if err != nil {
			s.logger.WarnContext(ctx, "invalid node.capabilities skipped", slog.String("node_id", n.ID().String()), slog.Any("error", err))

			return nil
		}

		platform, err := json.Marshal(caps.Platform)
		if err != nil {
			return err
		}

		rep, err := domain.NewCapabilityReport(n.ID(), hash, truncate(caps.ProductVersion, 32), caps.Protocols, platform, doc, now, rows)
		if err != nil {
			s.logger.WarnContext(ctx, "invalid node.capabilities skipped", slog.String("node_id", n.ID().String()), slog.Any("error", err))

			return nil
		}

		if err := s.repo.Replace(ctx, rep); err != nil {
			return err
		}

		for _, h := range s.onReport {
			if err := h(ctx, n, caps, now); err != nil {
				return err
			}
		}

		s.logger.InfoContext(ctx, "node capabilities updated", slog.String("node_id", n.ID().String()),
			slog.Int("capabilities", len(rows)), slog.Int("devices", len(caps.Devices)))

		return nil
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n]
}

// Get returns the last report of a node.
func (s *Capabilities) Get(ctx context.Context, id string) (domain.CapabilityReport, error) {
	nid, err := domain.NewNodeID(id)
	if err != nil {
		return domain.CapabilityReport{}, domain.ErrNodeNotFound
	}

	if _, err := s.nodes.Get(ctx, nid); err != nil {
		return domain.CapabilityReport{}, err
	}

	return s.repo.Get(ctx, nid)
}

// Probe asks a connected node to re-probe its capabilities.
func (s *Capabilities) Probe(ctx context.Context, id string) error {
	nid, err := domain.NewNodeID(id)
	if err != nil {
		return domain.ErrNodeNotFound
	}

	if _, err := s.nodes.Get(ctx, nid); err != nil {
		return err
	}

	if err := s.links.Probe(ctx, nid); err != nil {
		return fmt.Errorf("probe %s: %w", id, err)
	}

	return nil
}
