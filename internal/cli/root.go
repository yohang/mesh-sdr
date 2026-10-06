// Package cli defines the meshsdr command line interface: one binary, the
// bare role starts the process (meshsdr hub, meshsdr node) and admin tasks
// are subcommands of the role (meshsdr hub migrate).
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/log"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitFailure = 1
	// ExitConfig is EX_CONFIG (sysexits.h): invalid configuration
	// (TECHNICAL_SPEC §7.4 "Validation at startup").
	ExitConfig = 78
)

type app struct {
	stdin          io.Reader
	reader         *bufio.Reader
	stdout, stderr io.Writer
	// env is the environment; nil means the process environment.
	env map[string]string

	configDir      string
	debug          bool
	json           bool
	silent         bool
	noninteractive bool
}

// Execute runs the command line and returns the process exit code.
func Execute(ctx context.Context, args []string) int {
	a := &app{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}

	return a.execute(ctx, args)
}

func (a *app) execute(ctx context.Context, args []string) int {
	cmd := a.newRootCmd()
	cmd.SetArgs(args)
	cmd.SetOut(a.stdout)
	cmd.SetErr(a.stderr)

	err := cmd.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}

	a.printError(err)

	return exitCode(err)
}

func (a *app) newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "meshsdr",
		Short:         "MeshSDR: one hub and N SDR nodes",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	f := cmd.PersistentFlags()
	f.StringVarP(&a.configDir, "config-dir", "c", "", "config directory (default $"+config.EnvConfigDir+" or "+config.DefaultDir+")")
	f.BoolVar(&a.debug, "debug", false, "log at debug level (overrides log.level)")
	f.BoolVar(&a.json, "json", false, "machine-readable output and JSON logs (overrides log.format)")
	f.BoolVar(&a.silent, "silent", false, "print errors only")
	f.BoolVar(&a.noninteractive, "noninteractive", false, "never prompt (read secrets from the environment instead)")

	cmd.AddCommand(a.newHubCmd(), a.newNodeCmd())

	return cmd
}

// configOptions resolves the config directory: --config-dir, then
// MESHSDR_CONFIG_DIR, then /etc/meshsdr.
func (a *app) configOptions() config.Options {
	dir := a.configDir
	if dir == "" {
		if a.env != nil {
			dir = a.env[config.EnvConfigDir]
		} else {
			dir = os.Getenv(config.EnvConfigDir)
		}
	}

	return config.Options{Dir: dir, Env: a.env}
}

// newLogger builds the process logger from log.* and the global flags.
func (a *app) newLogger(cfg config.Log) (*slog.Logger, error) {
	level, format := cfg.Level, cfg.Format

	switch {
	case a.debug:
		level = "debug"
	case a.silent:
		level = "error"
	}

	if a.json {
		format = "json"
	}

	return log.New(a.stderr, level, format)
}

// logConfig logs the startup summary of a loaded configuration.
func logConfig(ctx context.Context, logger *slog.Logger, role config.Role, meta config.Meta) {
	for _, w := range meta.Warnings {
		logger.WarnContext(ctx, "configuration warning", slog.String("warning", w))
	}

	logger.InfoContext(ctx, "configuration loaded",
		slog.String("role", string(role)),
		slog.Any("files", meta.Files),
		slog.Int("locked_keys", meta.Origins.Locked()))
}

// print writes command output unless --silent.
func (a *app) print(format string, args ...any) {
	if !a.silent {
		_, _ = fmt.Fprintf(a.stdout, format+"\n", args...)
	}
}

// printJSON writes v as indented JSON.
func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")

	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("write output: %w", err)
	}

	return nil
}

func (a *app) printError(err error) {
	var status exitStatus
	if errors.As(err, &status) {
		return
	}

	if a.json {
		_ = json.NewEncoder(a.stderr).Encode(map[string]string{"error": err.Error(), "code": errorCode(err)})

		return
	}

	_, _ = fmt.Fprintln(a.stderr, "meshsdr: "+err.Error())
}

func exitCode(err error) int {
	var status exitStatus
	if errors.As(err, &status) {
		return int(status)
	}

	var cerr *config.Error
	if errors.As(err, &cerr) {
		return ExitConfig
	}

	return ExitFailure
}

func errorCode(err error) string {
	var cerr *config.Error

	for _, e := range []error{db.ErrMigrationsPending, db.ErrSchemaTooNew, db.ErrChecksumMismatch, db.ErrEngineUnsupported} {
		if errors.Is(err, e) {
			return e.Error()
		}
	}

	if errors.As(err, &cerr) {
		return "config_invalid"
	}

	var de *shared.Error
	if errors.As(err, &de) {
		return string(de.Code())
	}

	return "error"
}
