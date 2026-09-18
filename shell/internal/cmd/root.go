package cmd

import (
	"context"
	"errors"
	"os"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/spf13/cobra"

	"github.com/obtai/azd-extensions/shell/internal/exec"
	"github.com/obtai/azd-extensions/shell/internal/ui"
)

// NewRootCommand builds `azd shell`. The root does the work: there is one
// thing this extension does, so it has no verb of its own.
func NewRootCommand() *cobra.Command {
	// `Name` must agree with `namespace` in extension.yaml — azd invokes the
	// binary with the subcommand arguments only.
	rootCmd, extCtx := azdext.NewExtensionRootCommand(azdext.ExtensionCommandOptions{
		Name:  "shell",
		Use:   "shell",
		Short: "Open a shell in a running container app, picking the app, revision, replica and container.",
		Long: `Open a shell in a running container app.

Inside an azd project the apps come from the environment's resource group and
can be named by azd service. Anywhere else, pick a subscription and resource
group first. Any step with one choice is taken without asking; any flag skips
its step.`,
	})

	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true
	rootCmd.CompletionOptions = cobra.CompletionOptions{
		DisableDefaultCmd: true,
	}
	rootCmd.SetHelpCommand(&cobra.Command{Hidden: true})

	var opts options
	rootCmd.Args = cobra.NoArgs
	rootCmd.RunE = func(cmd *cobra.Command, _ []string) error {
		err := run(cmd.Context(), extCtx, opts)
		if errors.Is(err, ui.ErrAborted) {
			os.Exit(130)
		}
		return err
	}

	flags := rootCmd.Flags()
	flags.StringVarP(&opts.subscription, "subscription", "s", "", "Subscription ID (default: the azd environment's)")
	flags.StringVarP(&opts.resourceGroup, "resource-group", "g", "", "Resource group (default: the azd environment's)")
	flags.StringVarP(&opts.app, "app", "a", "", "Container app, by name or azd service name")
	flags.StringVarP(&opts.revision, "revision", "r", "", "Revision (default: pick from the active ones)")
	flags.StringVar(&opts.replica, "replica", "", "Replica (default: pick from the running ones)")
	flags.StringVarP(&opts.container, "container", "u", "", "Container (default: pick from the replica's)")
	flags.StringVarP(&opts.command, "command", "c", "/bin/sh", "Command to run in the container")
	flags.BoolVarP(&opts.quiet, "quiet", "q", false, "Print nothing but the container's own output")

	rootCmd.AddCommand(newVersionCommand(&extCtx.OutputFormat))
	rootCmd.AddCommand(newMetadataCommand(rootCmd))

	return rootCmd
}

func run(ctx context.Context, extCtx *azdext.ExtensionContext, opts options) error {
	u := ui.New(extCtx.NoPrompt)

	t, err := resolve(ctx, u, extCtx.Environment, opts)
	if err != nil {
		return err
	}

	token, err := t.clients.AuthToken(ctx, t.resourceGroup, t.app)
	if err != nil {
		return err
	}
	endpoint, err := exec.Endpoint(
		deref(t.container.ExecEndpoint), deref(t.container.LogStreamEndpoint),
		t.clients.SubscriptionID, t.resourceGroup, t.app, t.revision, t.replica,
		deref(t.container.Name), opts.command)
	if err != nil {
		return err
	}

	if !opts.quiet {
		ui.Path(t.app, t.revision, t.replica, deref(t.container.Name))
	}
	return exec.Run(ctx, endpoint, token, extCtx.Debug)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
