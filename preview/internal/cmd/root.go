// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/spf13/cobra"
)

// NewRootCommand builds `azd preview`.
//
// Previews and nothing else. A release's lifecycle — seeding secrets,
// migrating, gating on health — belongs to the repository being released, as
// azd hooks in its azure.yaml, because what those steps have to do differs per
// repository. A preview does not: it is the same shape everywhere, which is
// what makes it worth putting in a shared tool.
func NewRootCommand() *cobra.Command {
	// `Name` and `Use` are this binary's own help output. The name azd exposes
	// comes from `namespace` in extension.yaml, since azd invokes the binary
	// with the subcommand arguments only — they have to agree, or the help text
	// contradicts the command that produced it.
	rootCmd, extCtx := azdext.NewExtensionRootCommand(azdext.ExtensionCommandOptions{
		Name:  "preview",
		Use:   "preview <command> [options]",
		Short: "Per-pull-request preview environments on Azure Container Apps.",
	})

	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true
	rootCmd.CompletionOptions = cobra.CompletionOptions{
		DisableDefaultCmd: true,
	}

	rootCmd.SetHelpCommand(&cobra.Command{Hidden: true})

	rootCmd.AddCommand(newUpCommand())
	rootCmd.AddCommand(newDownCommand())
	rootCmd.AddCommand(newURLCommand())
	rootCmd.AddCommand(newVersionCommand(&extCtx.OutputFormat))
	rootCmd.AddCommand(newMetadataCommand(rootCmd))

	return rootCmd
}
