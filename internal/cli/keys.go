package cli

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/yohang/mesh-sdr/internal/identity/infra/keyring"
	"github.com/yohang/mesh-sdr/internal/wire"
)

func (a *app) newKeysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage the access token signing keys (auth.token_key_dir)",
		Long: "Manage the Ed25519 keys that sign access tokens. The running hub picks the\n" +
			"changes up within a minute and pushes the keys to the nodes.",
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List the keys and their state",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, _ []string) error { return a.keysList(cmd.Context()) },
		},
		&cobra.Command{
			Use:   "rotate",
			Short: "Introduce a new key: published now, signing after one minute",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, _ []string) error { return a.keysRotate(cmd.Context()) },
		},
		&cobra.Command{
			Use:   "revoke [--] <kid>",
			Short: "Revoke a key (compromise): nodes refuse its tokens at once",
			Long: "Revoke a key (compromise): nodes refuse its tokens at once.\n\n" +
				"Key ids are base64url and may start with '-': put \"--\" before the kid so it is not read as a flag.",
			Example: "  meshsdr hub keys revoke -- <kid>",
			Args:    cobra.ExactArgs(1),
			RunE:    func(cmd *cobra.Command, args []string) error { return a.keysRevoke(cmd.Context(), args[0]) },
		},
	)

	return cmd
}

func (a *app) withKeyring(ctx context.Context, fn func(k *keyring.Keyring, now time.Time) error) error {
	cfg, _, _, err := a.loadHub(ctx)
	if err != nil {
		return err
	}

	now := time.Now()

	k, err := wire.TokenKeyring(cfg, now)
	if err != nil {
		return err
	}

	return fn(k, now)
}

func keyState(k keyring.Key, now time.Time) string {
	switch {
	case k.Revoked:
		return "revoked"
	case k.Signing:
		return "signing"
	case k.ActivatesAt.After(now):
		return "next"
	default:
		return "retired"
	}
}

func (a *app) keysList(ctx context.Context) error {
	return a.withKeyring(ctx, func(k *keyring.Keyring, now time.Time) error {
		keys, err := k.List(now)
		if err != nil {
			return err
		}

		if a.json {
			out := make([]map[string]any, 0, len(keys))
			for _, key := range keys {
				out = append(out, map[string]any{
					"kid": key.Kid, "state": keyState(key, now), "created_at": key.CreatedAt, "activates_at": key.ActivatesAt,
				})
			}

			return a.printJSON(out)
		}

		tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "KID\tSTATE\tCREATED\tSIGNS FROM")

		for _, key := range keys {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", key.Kid, keyState(key, now), key.CreatedAt.UTC().Format(time.RFC3339),
				key.ActivatesAt.UTC().Format(time.RFC3339))
		}

		return tw.Flush()
	})
}

func (a *app) keysRotate(ctx context.Context) error {
	return a.withKeyring(ctx, func(k *keyring.Keyring, now time.Time) error {
		kid, err := k.Rotate(now)
		if err != nil {
			return err
		}

		if a.json {
			return a.printJSON(map[string]any{"kid": kid})
		}

		a.print("key %s added: published now, signing from %s", kid, now.Add(keyring.PublishLead).UTC().Format(time.RFC3339))

		return nil
	})
}

func (a *app) keysRevoke(ctx context.Context, kid string) error {
	return a.withKeyring(ctx, func(k *keyring.Keyring, now time.Time) error {
		if err := k.Revoke(kid, now); err != nil {
			return err
		}

		if a.json {
			return a.printJSON(map[string]any{"kid": kid, "revoked": true})
		}

		a.print("key %s revoked", kid)

		return nil
	})
}
