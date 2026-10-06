package wire

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"slices"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	gridinfra "github.com/yohang/mesh-sdr/internal/grid/infra"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
)

// errGridDisabled is returned by grid features that need the hub CA.
var errGridDisabled = domain.ErrNodeUnavailable.WithDetail("the hub internal CA is not configured (tls.ca_cert): the grid is disabled")

// caInfo adapts the optional hub CA to app.CAInfo.
type caInfo struct{ ca *pki.CA }

func (c caInfo) Fingerprint() (string, error) {
	if c.ca == nil {
		return "", errGridDisabled
	}

	return pki.FormatFingerprint(c.ca.Fingerprint()), nil
}

// hubGrid is the hub side of the grid module.
type hubGrid struct {
	ca      *pki.CA
	hubID   string
	nodes   *app.Nodes
	startup []func(ctx context.Context) error
	workers []func(ctx context.Context)
}

// HubID returns the hub id: the host of hub.url (ADR 0008 Q6).
func HubID(hubURL string) (string, error) {
	u, err := url.Parse(hubURL)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("hub.url %q has no host", hubURL)
	}

	return u.Hostname(), nil
}

// LoadCA reads the hub CA from the [tls] config, or returns nil when the
// grid is disabled.
func LoadCA(cfg config.HubTLS) (*pki.CA, error) {
	if cfg.CACert == "" {
		return nil, nil
	}

	certPEM, err := os.ReadFile(cfg.CACert)
	if err != nil {
		return nil, fmt.Errorf("tls.ca_cert: %w", err)
	}

	ca, err := pki.ParseCA(certPEM, []byte(cfg.CAKey.Reveal()))
	if err != nil {
		return nil, fmt.Errorf("tls.ca_cert / tls.ca_key: %w", err)
	}

	return ca, nil
}

// DeclaredNodes converts the [nodes.<id>] tables.
func DeclaredNodes(cfg map[string]config.ConfigNode) ([]app.DeclaredNode, error) {
	out := make([]app.DeclaredNode, 0, len(cfg))

	for _, id := range slices.Sorted(maps.Keys(cfg)) {
		c := cfg[id]

		nid, err := domain.NewNodeID(id)
		if err != nil {
			return nil, fmt.Errorf("nodes.%s: %w", id, err)
		}

		nameStr := c.Name
		if nameStr == "" {
			nameStr = id
		}

		name, err := domain.NewNodeName(nameStr)
		if err != nil {
			return nil, fmt.Errorf("nodes.%s.name: %w", id, err)
		}

		u, err := domain.NewNodeURL(c.URL)
		if err != nil {
			return nil, fmt.Errorf("nodes.%s.url: %w", id, err)
		}

		d := app.DeclaredNode{ID: nid, Name: name, URL: u}

		if c.EnrollmentToken.IsSet() {
			tok, err := domain.ParseEnrollmentToken(c.EnrollmentToken.Reveal())
			if err != nil {
				return nil, fmt.Errorf("nodes.%s.enrollment_token: %w", id, err)
			}

			d.Token = &tok
		}

		out = append(out, d)
	}

	return out, nil
}

func newHubGrid(cfg config.Hub, logger *slog.Logger, adapter db.Adapter, now func() time.Time) (*hubGrid, error) {
	ca, err := LoadCA(cfg.TLS)
	if err != nil {
		return nil, err
	}

	hubID, err := HubID(cfg.Hub.URL)
	if err != nil {
		return nil, err
	}

	declared, err := DeclaredNodes(cfg.Nodes)
	if err != nil {
		return nil, err
	}

	timings := app.DefaultTimings()
	audit := gridinfra.NewLogAuditor(component(logger, "grid.infra.audit"))
	nodeRepo := gridsqlite.NewNodeRepository(adapter)
	revocations := gridsqlite.NewRevocationRepository(adapter)

	g := &hubGrid{
		ca:    ca,
		hubID: hubID,
		nodes: app.NewNodes(nodeRepo, revocations, adapter, audit, caInfo{ca: ca}, timings, now, component(logger, "grid.app.nodes")),
	}

	gridLogger := component(logger, "grid.wire")

	g.startup = append(g.startup, func(ctx context.Context) error {
		if ca == nil {
			gridLogger.WarnContext(ctx, "hub internal CA not configured (tls.ca_cert): node enrollment and control channels are disabled; run `meshsdr hub ca init`")
		} else {
			gridLogger.InfoContext(ctx, "hub internal CA loaded",
				slog.String("hub_id", hubID), slog.String("ca_fingerprint", pki.FormatFingerprint(ca.Fingerprint())))
		}

		return g.nodes.SyncConfig(ctx, declared)
	})

	return g, nil
}
