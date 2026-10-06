package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yohang/mesh-sdr/internal/config"
)

func (a *app) newConfigCmd(role config.Role) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect the " + string(role) + " configuration",
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "schema",
			Short: "Print the JSON Schema (draft 2020-12) of " + string(role) + ".toml",
			Args:  cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error {
				b, err := config.Schema(role)
				if err != nil {
					return err
				}

				_, err = fmt.Fprintln(a.stdout, string(b))

				return err
			},
		},
		&cobra.Command{
			Use:   "check",
			Short: "Load and validate the " + string(role) + " configuration without starting",
			Args:  cobra.NoArgs,
			RunE:  func(*cobra.Command, []string) error { return a.configCheck(role) },
		},
	)

	return cmd
}

func (a *app) configCheck(role config.Role) error {
	var (
		meta config.Meta
		err  error
	)

	opts := a.configOptions()

	switch role {
	case config.RoleHub:
		_, meta, err = config.LoadHub(opts)
	case config.RoleNode:
		_, meta, err = config.LoadNode(opts)
	}

	if err != nil {
		return err
	}

	origins := map[string]string{}

	for _, k := range meta.Origins.Keys() {
		origins[k] = meta.Origins.Of(k).String()
	}

	if a.json {
		return a.printJSON(map[string]any{
			"valid":    true,
			"role":     role,
			"files":    meta.Files,
			"origins":  origins,
			"warnings": meta.Warnings,
		})
	}

	a.print("%s configuration is valid (files: %s)", role, strings.Join(meta.Files, ", "))

	for _, k := range meta.Origins.Keys() {
		a.print("  %-32s %s", k, origins[k])
	}

	for _, w := range meta.Warnings {
		a.print("warning: %s", w)
	}

	return nil
}
