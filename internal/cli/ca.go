package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
)

func (a *app) newCACmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ca",
		Short: "Manage the hub internal CA (hub <-> node mTLS)",
		Args:  cobra.NoArgs,
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "Create the hub internal CA in <config-dir>/tls (never overwrites)",
		Long: "Create the hub internal CA: <config-dir>/tls/ca.pem and ca.key (0600).\n" +
			"The command refuses to run when either file exists. Reference them in hub.toml:\n\n" +
			"  [tls]\n  ca_cert = \"tls/ca.pem\"\n  ca_key = { file = \"tls/ca.key\" }",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.caInit() },
	})

	return cmd
}

type caInitJSON struct {
	CACert      string `json:"ca_cert"`
	CAKey       string `json:"ca_key"`
	Fingerprint string `json:"fingerprint"`
}

func (a *app) caInit() error {
	dir := a.configOptions().Dir
	if dir == "" {
		dir = config.DefaultDir
	}

	certPath := filepath.Join(dir, "tls", "ca.pem")
	keyPath := filepath.Join(dir, "tls", "ca.key")

	certPEM, keyPEM, err := pki.GenerateCA("MeshSDR hub CA", time.Now())
	if err != nil {
		return err
	}

	// Exclusive creation: an existing CA is never replaced, even by a
	// concurrent run.
	if err := pki.WriteFileExclusive(keyPath, keyPEM, 0o600); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists: refusing to overwrite the hub CA", keyPath)
		}

		return err
	}

	if err := pki.WriteFileExclusive(certPath, certPEM, 0o644); err != nil {
		_ = os.Remove(keyPath)

		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists: refusing to overwrite the hub CA", certPath)
		}

		return err
	}

	ca, err := pki.ParseCA(certPEM, keyPEM)
	if err != nil {
		return err
	}

	fp := pki.FormatFingerprint(ca.Fingerprint())

	if a.json {
		return a.printJSON(caInitJSON{CACert: certPath, CAKey: keyPath, Fingerprint: fp})
	}

	a.print("created %s and %s", certPath, keyPath)
	a.print("CA fingerprint (SHA-256): %s", fp)
	a.print("add to hub.toml:\n\n[tls]\nca_cert = \"tls/ca.pem\"\nca_key = { file = \"tls/ca.key\" }")

	return nil
}
