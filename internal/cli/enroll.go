package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/enroll"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	"github.com/yohang/mesh-sdr/internal/wire"
)

type enrollFlags struct {
	tokenFile     string
	caFingerprint string
	timeout       time.Duration
	force         bool
}

func (a *app) newEnrollCmd() *cobra.Command {
	var f enrollFlags

	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "Wait for the hub to enroll this node, then write its certificate and key",
		Long: "Serve POST /enroll on node.listen until the hub enrolls the node (TECHNICAL_SPEC §4.2),\n" +
			"then write tls.key (0600), tls.cert and hub_trust.ca_cert and exit. The token comes from\n" +
			"--token-file or hub_trust.enrollment_token, the CA fingerprint from --ca-fingerprint or\n" +
			"hub_trust.ca_fingerprint; both are shown by the hub when the node is added.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runEnroll(cmd.Context(), f) },
	}

	cmd.Flags().StringVar(&f.tokenFile, "token-file", "", "file holding the enrollment token (overrides hub_trust.enrollment_token)")
	cmd.Flags().StringVar(&f.caFingerprint, "ca-fingerprint", "", "SHA-256 fingerprint of the hub CA (overrides hub_trust.ca_fingerprint)")
	cmd.Flags().DurationVar(&f.timeout, "timeout", time.Hour, "give up after this delay")
	cmd.Flags().BoolVar(&f.force, "force", false, "re-enroll: overwrite an existing tls.cert and tls.key")

	return cmd
}

func (a *app) runEnroll(ctx context.Context, f enrollFlags) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, logger, err := a.loadNode(ctx)
	if err != nil {
		return err
	}

	if cfg.TLS.Cert == "" || cfg.TLS.Key == "" || cfg.HubTrust.CACert == "" {
		return errors.New("node enroll needs tls.cert, tls.key and hub_trust.ca_cert in the node config")
	}

	if !f.force {
		for _, p := range []string{cfg.TLS.Cert, cfg.TLS.Key} {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%s exists: the node is already enrolled (use --force to re-enroll)", p)
			} else if !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("check %s: %w", p, err)
			}
		}
	}

	token, err := enrollToken(cfg, f.tokenFile)
	if err != nil {
		return err
	}

	fpText := f.caFingerprint
	if fpText == "" {
		fpText = cfg.HubTrust.CAFingerprint
	}

	if fpText == "" {
		return errors.New("the hub CA fingerprint is required: --ca-fingerprint or hub_trust.ca_fingerprint")
	}

	fp, err := pki.ParseFingerprint(fpText)
	if err != nil {
		return fmt.Errorf("CA fingerprint: %w", err)
	}

	e, err := wire.NodeEnrollment(cfg, logger, token, fp, time.Now)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	runErr := make(chan error, 1)

	go func() { runErr <- e.Run(ctx) }()

	logger.InfoContext(ctx, "waiting for the hub to enroll this node",
		slog.String("node_id", cfg.Node.ID), slog.String("listen", cfg.Node.Listen))

	select {
	case res := <-e.Enroller.Done():
		werr := enroll.WriteFiles(e.Paths, e.Key, res.CA.Raw, res.Chain...)

		cancel()
		<-runErr

		if werr != nil {
			return werr
		}

		logger.InfoContext(ctx, "node enrolled", slog.String("cert", e.Paths.Cert), slog.String("key", e.Paths.Key))
		a.print("node %s enrolled: certificate written to %s", cfg.Node.ID, e.Paths.Cert)

		return nil
	case err := <-runErr:
		if err == nil {
			err = ctx.Err()
		}

		return fmt.Errorf("enrollment did not complete: %w", err)
	}
}

func enrollToken(cfg config.Node, tokenFile string) (domain.EnrollmentToken, error) {
	raw := cfg.HubTrust.EnrollmentToken.Reveal()

	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile) //nolint:gosec // operator-provided path
		if err != nil {
			return domain.EnrollmentToken{}, fmt.Errorf("token file: %w", err)
		}

		raw = strings.TrimSpace(string(b))
	}

	if raw == "" {
		return domain.EnrollmentToken{}, errors.New("the enrollment token is required: --token-file or hub_trust.enrollment_token")
	}

	tok, err := domain.ParseEnrollmentToken(raw)
	if err != nil {
		return domain.EnrollmentToken{}, fmt.Errorf("enrollment token: %w", err)
	}

	return tok, nil
}
