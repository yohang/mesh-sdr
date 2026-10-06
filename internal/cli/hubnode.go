package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/wire"
)

func (a *app) newHubNodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Manage nodes (add, list, show, token, disable, enable, remove)",
		Args:  cobra.NoArgs,
	}

	var name, url string

	add := &cobra.Command{
		Use:   "add <id> --url https://host:port",
		Short: "Declare a node and print its enrollment token (shown once) and the hub CA fingerprint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withNodes(cmd.Context(), func(ctx context.Context, s *gridapp.Nodes) error {
				issued, err := s.Add(ctx, gridapp.ActorCLI, gridapp.NewNodeInput{ID: args[0], Name: name, URL: url})
				if err != nil {
					return err
				}

				return a.printIssued(issued)
			})
		},
	}
	add.Flags().StringVar(&url, "url", "", "base URL the hub dials: https://host:port (required)")
	add.Flags().StringVar(&name, "name", "", "display name (defaults to the id)")
	_ = add.MarkFlagRequired("url")

	cmd.AddCommand(add,
		&cobra.Command{
			Use:   "list",
			Short: "List nodes",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return a.withNodes(cmd.Context(), func(ctx context.Context, s *gridapp.Nodes) error {
					nodes, err := s.List(ctx)
					if err != nil {
						return err
					}

					if a.json {
						out := make([]nodeJSON, 0, len(nodes))
						for _, n := range nodes {
							out = append(out, toNodeJSON(n))
						}

						return a.printJSON(out)
					}

					for _, n := range nodes {
						j := toNodeJSON(n)
						a.print("%-24s %-9s %-12s %-6s %s", j.ID, j.EnrollmentState, j.Status, j.Origin, j.URL)
					}

					return nil
				})
			},
		},
		&cobra.Command{
			Use:   "show <id>",
			Short: "Show a node",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.withNodes(cmd.Context(), func(ctx context.Context, s *gridapp.Nodes) error {
					n, err := s.Get(ctx, args[0])
					if err != nil {
						return err
					}

					return a.printJSON(toNodeJSON(n))
				})
			},
		},
		&cobra.Command{
			Use:   "token <id>",
			Short: "Issue a new enrollment token (re-enrollment; revokes the current certificate)",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.withNodes(cmd.Context(), func(ctx context.Context, s *gridapp.Nodes) error {
					issued, err := s.IssueToken(ctx, gridapp.ActorCLI, args[0])
					if err != nil {
						return err
					}

					return a.printIssued(issued)
				})
			},
		},
		a.newNodeToggleCmd("disable", true),
		a.newNodeToggleCmd("enable", false),
		&cobra.Command{
			Use:   "remove <id>",
			Short: "Remove an admin-added node and revoke its certificate",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.withNodes(cmd.Context(), func(ctx context.Context, s *gridapp.Nodes) error {
					if err := s.Delete(ctx, gridapp.ActorCLI, args[0]); err != nil {
						return err
					}

					a.print("node %s removed", args[0])

					return nil
				})
			},
		},
	)

	return cmd
}

func (a *app) newNodeToggleCmd(verb string, disabled bool) *cobra.Command {
	return &cobra.Command{
		Use:   verb + " <id>",
		Short: verb + " a node (the hub keeps no control channel to a disabled node)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withNodes(cmd.Context(), func(ctx context.Context, s *gridapp.Nodes) error {
				n, err := s.Get(ctx, args[0])
				if err != nil {
					return err
				}

				if _, err := s.Update(ctx, gridapp.ActorCLI, args[0], nil, nil, &disabled, n.Version()); err != nil {
					return err
				}

				a.print("node %s %sd", args[0], verb)

				return nil
			})
		},
	}
}

func (a *app) withNodes(ctx context.Context, fn func(context.Context, *gridapp.Nodes) error) error {
	cfg, _, logger, err := a.loadHub(ctx)
	if err != nil {
		return err
	}

	adapter, err := wire.OpenDB(ctx, cfg.DB, logger)
	if err != nil {
		return err
	}

	defer func() { _ = adapter.Close() }()

	if err := adapter.Migrator().Check(ctx); err != nil {
		return fmt.Errorf("database not ready: %w", err)
	}

	s, err := wire.HubNodes(cfg, logger, adapter)
	if err != nil {
		return err
	}

	return fn(ctx, s)
}

type issuedJSON struct {
	Node            nodeJSON   `json:"node"`
	EnrollmentToken string     `json:"enrollment_token"`
	CAFingerprint   string     `json:"ca_fingerprint"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
}

func (a *app) printIssued(i gridapp.Issued) error {
	var exp *time.Time
	if !i.ExpiresAt.IsZero() {
		exp = &i.ExpiresAt
	}

	if a.json {
		return a.printJSON(issuedJSON{Node: toNodeJSON(i.Node), EnrollmentToken: i.Token.String(), CAFingerprint: i.CAFingerprint, ExpiresAt: exp})
	}

	a.print("node %s is waiting for enrollment.", i.Node.ID())
	a.print("enrollment token (shown once): %s", i.Token)
	a.print("hub CA fingerprint:           %s", i.CAFingerprint)

	if exp != nil {
		a.print("expires at:                   %s", exp.Format(time.RFC3339))
	}

	a.print("on the node: meshsdr node enroll --token-file <file> --ca-fingerprint %s", i.CAFingerprint)

	return nil
}

type nodeJSON struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	URL             string     `json:"url"`
	Origin          string     `json:"origin"`
	Disabled        bool       `json:"disabled"`
	EnrollmentState string     `json:"enrollment_state"`
	Status          string     `json:"status"`
	StatusHint      string     `json:"status_hint,omitempty"`
	CertSerial      string     `json:"cert_serial,omitempty"`
	CertNotAfter    *time.Time `json:"cert_not_after,omitempty"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	SoftwareVersion string     `json:"software_version,omitempty"`
	Version         int        `json:"version"`
}

func toNodeJSON(n *domain.Node) nodeJSON {
	s := n.Snapshot()
	out := nodeJSON{
		ID: s.ID, Name: s.Name, URL: s.URL, Origin: string(s.Origin), Disabled: s.Disabled,
		EnrollmentState: string(s.Enrollment), Status: string(s.Runtime.Status), StatusHint: s.Runtime.StatusHint,
		CertSerial: s.CertSerial, SoftwareVersion: s.Runtime.SoftwareVersion, Version: s.Version,
	}

	if !s.CertNotAfter.IsZero() {
		t := s.CertNotAfter
		out.CertNotAfter = &t
	}

	if !s.Runtime.LastHeartbeatAt.IsZero() {
		t := s.Runtime.LastHeartbeatAt
		out.LastHeartbeatAt = &t
	}

	return out
}
