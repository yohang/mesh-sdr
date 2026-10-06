package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/yohang/mesh-sdr/internal/config"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/wire"
)

// exitStatus ends a command with a status code and no error message
// (`hub user exists`).
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func (a *app) newUserCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage hub user accounts (on the hub host)",
		Long: "Manage user accounts directly in the hub database. The schema must be\n" +
			"current (`meshsdr hub migrate`).",
	}

	var add struct {
		email, displayName, role string
	}

	addCmd := &cobra.Command{
		Use:   "add <username>",
		Short: "Create a user with a local password",
		Long: "Create a user. Interactively, the password is asked twice. With\n" +
			"--noninteractive it is read from $" + config.EnvPassword + "; when no password is\n" +
			"given, a random one is generated, printed once and must be changed at the\n" +
			"first sign-in.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.userAdd(cmd.Context(), args[0], add.email, add.displayName, add.role)
		},
	}
	addCmd.Flags().StringVar(&add.email, "email", "", "e-mail address")
	addCmd.Flags().StringVar(&add.displayName, "display-name", "", "display name")
	addCmd.Flags().StringVar(&add.role, "role", "listener", "role: listener, operator or admin")

	var listAll bool

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List users: name, e-mail, roles, enabled state and last sign-in",
		Long: "List the enabled users by username, or every user with --all. --json prints\n" +
			"an array of objects.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.userList(cmd.Context(), listAll) },
	}
	listCmd.Flags().BoolVar(&listAll, "all", false, "include disabled users")

	cmd.AddCommand(
		addCmd,
		listCmd,
		&cobra.Command{
			Use:   "exists <username>",
			Short: "Exit with status 0 when the user exists, 1 otherwise",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return a.userExists(cmd.Context(), args[0]) },
		},
		&cobra.Command{
			Use:   "reset-password <username>",
			Short: "Set a new password and revoke all the user's sessions",
			Long: "Set a new password, like `user add`: asked twice interactively, read from\n" +
				"$" + config.EnvPassword + " with --noninteractive, or generated, printed once and to be\n" +
				"changed at the next sign-in. Every session of the user is revoked and the\n" +
				"login lock-out is cleared.",
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error { return a.userResetPassword(cmd.Context(), args[0]) },
		},
		&cobra.Command{
			Use:   "disable <username>",
			Short: "Disable a user and revoke all its sessions",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return a.userDisable(cmd.Context(), args[0]) },
		},
		&cobra.Command{
			Use:   "enable <username>",
			Short: "Enable a disabled user",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return a.userEnable(cmd.Context(), args[0]) },
		},
	)

	return cmd
}

// withUserAdmin opens the hub database, checks its schema and calls fn.
func (a *app) withUserAdmin(ctx context.Context, fn func(*identityapp.UserAdmin) error) error {
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
		return fmt.Errorf("schema is not current: %w", err)
	}

	return fn(wire.UserAdmin(cfg, logger, adapter))
}

func (a *app) userAdd(ctx context.Context, name, email, displayName, roleName string) error {
	role, err := domain.ParseRole(roleName)
	if err != nil || role == domain.RoleAnonymous {
		return fmt.Errorf("--role %q: want listener, operator or admin", roleName)
	}

	password, err := a.newPassword()
	if err != nil {
		return err
	}

	return a.withUserAdmin(ctx, func(s *identityapp.UserAdmin) error {
		res, err := s.Add(ctx, identityapp.AddUserInput{
			Username: name, Email: email, DisplayName: displayName, Role: role, Password: password,
		})
		if err != nil {
			return err
		}

		u := res.User

		if a.json {
			out := map[string]any{
				"id": u.ID().String(), "username": u.Username().String(), "role": u.Role().String(),
				"must_change_password": u.MustChangePassword(),
			}
			if res.GeneratedPassword != "" {
				out["password"] = res.GeneratedPassword
			}

			return a.printJSON(out)
		}

		a.print("user %s created (role %s)", u.Username(), u.Role())

		// The generated password is printed even with --silent: it is shown
		// only once.
		if res.GeneratedPassword != "" {
			_, err := fmt.Fprintf(a.stdout, "password: %s\nIt must be changed at the first sign-in.\n", res.GeneratedPassword)

			return err
		}

		return nil
	})
}

func (a *app) userResetPassword(ctx context.Context, name string) error {
	password, err := a.newPassword()
	if err != nil {
		return err
	}

	return a.withUserAdmin(ctx, func(s *identityapp.UserAdmin) error {
		res, err := s.ResetPassword(ctx, name, password)
		if err != nil {
			return err
		}

		u := res.User

		if a.json {
			out := map[string]any{
				"username": u.Username().String(), "must_change_password": u.MustChangePassword(),
				"revoked_sessions": res.RevokedSessions,
			}
			if res.GeneratedPassword != "" {
				out["password"] = res.GeneratedPassword
			}

			return a.printJSON(out)
		}

		a.print("password of %s reset, %d session(s) revoked", u.Username(), res.RevokedSessions)

		// Like `user add`, a generated password is printed even with
		// --silent: it is shown only once.
		if res.GeneratedPassword != "" {
			_, err := fmt.Fprintf(a.stdout, "password: %s\nIt must be changed at the next sign-in.\n", res.GeneratedPassword)

			return err
		}

		return nil
	})
}

// newPassword returns the password of a new user: asked twice
// interactively, read from MESHSDR_PASSWORD with --noninteractive, or empty
// (generated by the service).
func (a *app) newPassword() (string, error) {
	if a.noninteractive {
		return a.getenv(config.EnvPassword), nil
	}

	first, err := a.prompt("Password (empty to generate one): ")
	if err != nil {
		return "", err
	}

	if first == "" {
		return "", nil
	}

	second, err := a.prompt("Repeat the password: ")
	if err != nil {
		return "", err
	}

	if first != second {
		return "", errors.New("the passwords do not match")
	}

	return first, nil
}

func (a *app) getenv(name string) string {
	if a.env != nil {
		return a.env[name]
	}

	return os.Getenv(name)
}

// prompt reads a line without echo from a terminal, or a plain line from a
// pipe.
func (a *app) prompt(label string) (string, error) {
	_, _ = fmt.Fprint(a.stderr, label)

	if f, ok := a.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		_, _ = fmt.Fprintln(a.stderr)

		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}

		return string(b), nil
	}

	if a.reader == nil {
		in := a.stdin
		if in == nil {
			in = strings.NewReader("")
		}

		a.reader = bufio.NewReader(in)
	}

	line, err := a.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password: %w", err)
	}

	return strings.TrimRight(line, "\r\n"), nil
}

func (a *app) userExists(ctx context.Context, name string) error {
	return a.withUserAdmin(ctx, func(s *identityapp.UserAdmin) error {
		ok, err := s.Exists(ctx, name)
		if err != nil {
			return err
		}

		if a.json {
			if err := a.printJSON(map[string]any{"username": name, "exists": ok}); err != nil {
				return err
			}
		}

		if !ok {
			return exitStatus(ExitFailure)
		}

		return nil
	})
}

// roleNames lists the roles of a user: listener (implicit), then each
// grant, with its device scope after '@'.
func roleNames(u *domain.User) []string {
	names := []string{domain.RoleListener.String()}

	for _, g := range u.Grants() {
		n := g.Role().String()
		if !g.Global() {
			n += "@" + g.Device().String()
		}

		names = append(names, n)
	}

	return names
}

func (a *app) userList(ctx context.Context, all bool) error {
	return a.withUserAdmin(ctx, func(s *identityapp.UserAdmin) error {
		users, err := s.List(ctx, all)
		if err != nil {
			return err
		}

		if a.json {
			out := make([]map[string]any, 0, len(users))

			for _, u := range users {
				var email, lastLogin any
				if !u.Email().IsZero() {
					email = u.Email().String()
				}

				if t := u.LastLoginAt(); !t.IsZero() {
					lastLogin = t.UTC().Format(time.RFC3339)
				}

				out = append(out, map[string]any{
					"id": u.ID().String(), "username": u.Username().String(), "email": email,
					"roles": roleNames(u), "enabled": u.Enabled(), "must_change_password": u.MustChangePassword(),
					"last_login_at": lastLogin,
				})
			}

			return a.printJSON(out)
		}

		tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "USERNAME\tE-MAIL\tROLES\tENABLED\tLAST SIGN-IN")

		for _, u := range users {
			email, lastLogin := "-", "never"
			if !u.Email().IsZero() {
				email = u.Email().String()
			}

			if t := u.LastLoginAt(); !t.IsZero() {
				lastLogin = t.UTC().Format(time.RFC3339)
			}

			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", u.Username(), email, strings.Join(roleNames(u), ","),
				map[bool]string{true: "yes", false: "no"}[u.Enabled()], lastLogin)
		}

		return tw.Flush()
	})
}

func (a *app) userDisable(ctx context.Context, name string) error {
	return a.withUserAdmin(ctx, func(s *identityapp.UserAdmin) error {
		res, err := s.Disable(ctx, name)
		if err != nil {
			return err
		}

		if a.json {
			return a.printJSON(map[string]any{"username": name, "changed": res.Changed, "revoked_sessions": res.RevokedSessions})
		}

		if res.Changed {
			a.print("user %s disabled, %d session(s) revoked", name, res.RevokedSessions)
		} else {
			a.print("user %s is already disabled", name)
		}

		return nil
	})
}

func (a *app) userEnable(ctx context.Context, name string) error {
	return a.withUserAdmin(ctx, func(s *identityapp.UserAdmin) error {
		changed, err := s.Enable(ctx, name)
		if err != nil {
			return err
		}

		if a.json {
			return a.printJSON(map[string]any{"username": name, "changed": changed})
		}

		if changed {
			a.print("user %s enabled", name)
		} else {
			a.print("user %s is already enabled", name)
		}

		return nil
	})
}
