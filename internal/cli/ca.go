package cli

import (
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
		Long: "Create the hub internal CA: <config-dir>/" + config.CACertFile + " and " + config.CAKeyFile + " (0600).\n" +
			"The command refuses to run when either file exists. Reference them in hub.toml:\n\n" +
			"  [tls]\n  ca_cert = \"" + config.CACertFile + "\"\n  ca_key = { file = \"" + config.CAKeyFile + "\" }",
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
	dir := a.configOptions().ResolvedDir()
	certPath := filepath.Join(dir, config.CACertFile)
	keyPath := filepath.Join(dir, config.CAKeyFile)

	ca, err := pki.CreateCA(certPath, keyPath, time.Now())
	if err != nil {
		return err
	}

	fp := pki.FormatFingerprint(ca.Fingerprint())

	if a.json {
		return a.printJSON(caInitJSON{CACert: certPath, CAKey: keyPath, Fingerprint: fp})
	}

	a.print("created %s and %s", certPath, keyPath)
	a.print("CA fingerprint (SHA-256): %s", fp)
	a.print("add to hub.toml:\n\n[tls]\nca_cert = %q\nca_key = { file = %q }", config.CACertFile, config.CAKeyFile)

	return nil
}
