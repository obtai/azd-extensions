// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/spf13/cobra"
)

func NewRootCommand() *cobra.Command {
	// `Name` and `Use` here are cosmetic — they set this binary's own help
	// output. The name azd exposes comes from `namespace` in extension.yaml,
	// because azd invokes the binary with the subcommand arguments only. They
	// have to agree, or the help text contradicts the command that produced it.
	rootCmd, extCtx := azdext.NewExtensionRootCommand(azdext.ExtensionCommandOptions{
		Name:  "obt",
		Use:   "obt <command> [options]",
		Short: "Release lifecycle and per-PR previews for Azure Container Apps.",
	})

	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true
	rootCmd.CompletionOptions = cobra.CompletionOptions{
		DisableDefaultCmd: true,
	}

	rootCmd.SetHelpCommand(&cobra.Command{Hidden: true})

	// The four lifecycle handlers register no commands of their own — azd
	// starts this binary with `listen` and talks to it over gRPC.
	rootCmd.AddCommand(newListenCommand())
	rootCmd.AddCommand(newPreviewCommand())
	rootCmd.AddCommand(newVersionCommand(&extCtx.OutputFormat))
	rootCmd.AddCommand(newMetadataCommand(rootCmd))

	return rootCmd
}

// newPreviewCommand groups the preview commands under `azd obt preview`, rather
// than promoting them to `azd obt up|down|url` — which would read as though they
// applied to the whole deployment, and would take the names the release
// commands this namespace exists to make room for will want.
func newPreviewCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "preview <command> [options]",
		Short: "Per-pull-request preview environments.",
	}

	command.AddCommand(newUpCommand())
	command.AddCommand(newDownCommand())
	command.AddCommand(newURLCommand())

	return command
}
