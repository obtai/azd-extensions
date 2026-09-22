package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/spf13/cobra"

	"github.com/obtai/azd-extensions/db/internal/psql"
	"github.com/obtai/azd-extensions/db/internal/ui"
)

// NewRootCommand builds `azd db`. The root does the work: there is one thing
// this extension does, so it has no verb of its own.
func NewRootCommand() *cobra.Command {
	// `Name` must agree with `namespace` in extension.yaml — azd invokes the
	// binary with the subcommand arguments only.
	rootCmd, extCtx := azdext.NewExtensionRootCommand(azdext.ExtensionCommandOptions{
		Name:  "db",
		Use:   "db [options] [-- psql args]",
		Short: "Open psql on the PostgreSQL flexible server behind this app.",
		Long: `Open psql on the PostgreSQL flexible server behind this app.

Inside an azd project the server comes from the environment's resource group.
Anywhere else, pick a subscription and resource group first. Any step with one
choice is taken without asking; any flag skips its step.

The password is a Microsoft Entra token and the role name is your own UPN, so
nothing here needs a stored secret. Anything after -- goes to psql untouched.`,
	})

	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true
	rootCmd.CompletionOptions = cobra.CompletionOptions{
		DisableDefaultCmd: true,
	}
	rootCmd.SetHelpCommand(&cobra.Command{Hidden: true})

	var opts options
	// Args are psql's, not ours: everything is expected after --.
	rootCmd.Args = func(cmd *cobra.Command, args []string) error {
		if cmd.ArgsLenAtDash() > 0 {
			return fmt.Errorf("unexpected argument %q; psql arguments go after --", args[0])
		}
		if cmd.ArgsLenAtDash() == -1 && len(args) > 0 {
			return fmt.Errorf("unexpected argument %q; psql arguments go after --", args[0])
		}
		return nil
	}
	rootCmd.RunE = func(cmd *cobra.Command, args []string) error {
		err := run(cmd.Context(), extCtx, opts, args)
		if errors.Is(err, ui.ErrAborted) {
			os.Exit(130)
		}
		var exit *psql.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}
		return err
	}

	flags := rootCmd.Flags()
	flags.StringVarP(&opts.subscription, "subscription", "s", "", "Subscription ID (default: the azd environment's)")
	flags.StringVarP(&opts.resourceGroup, "resource-group", "g", "", "Resource group (default: the azd environment's)")
	flags.StringVar(&opts.server, "server", "", "Flexible server name (default: pick from the ones in scope)")
	flags.StringVarP(&opts.database, "database", "d", "", "Database (default: pick from the server's)")
	flags.StringVarP(&opts.user, "user", "U", "", "Postgres role (default: the Entra token's user)")
	flags.StringVarP(&opts.command, "command", "c", "", "Run one SQL statement and exit")
	flags.BoolVar(&opts.url, "url", false, "Print a connection URL on stdout instead of running psql")
	flags.BoolVarP(&opts.quiet, "quiet", "q", false, "Print nothing but psql's own output")

	rootCmd.AddCommand(newVersionCommand(&extCtx.OutputFormat))
	rootCmd.AddCommand(newMetadataCommand(rootCmd))

	return rootCmd
}

func run(ctx context.Context, extCtx *azdext.ExtensionContext, opts options, passthrough []string) error {
	u := ui.New(extCtx.NoPrompt)

	t, err := resolve(ctx, u, extCtx.Environment, opts)
	if err != nil {
		return err
	}

	token, err := psql.Token(ctx, t.credential, t.tenant)
	if err != nil {
		return err
	}
	user := opts.user
	if user == "" {
		user, err = psql.User(token)
		if err != nil {
			return err
		}
	}

	connection := psql.Connection{Host: t.host, Database: t.database, User: user, Token: token}

	// --url is a value, not a session: stdout carries the URL and nothing else,
	// so DATABASE_URL=$(azd db --url) works.
	if opts.url {
		fmt.Println(connection.URL())
		return nil
	}

	checkFirewall(ctx, u, t.clients, t.resourceGroup, t.server, user)

	if !opts.quiet {
		ui.Path(t.server, t.database, user)
	}
	err = connection.Run(ctx, opts.command, passthrough)
	var exit *psql.ExitError
	if errors.As(err, &exit) && exit.Code == psqlConnectionFailure {
		explain(ctx, t.clients, t.resourceGroup, t.server, user)
	}
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
