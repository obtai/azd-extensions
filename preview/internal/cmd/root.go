// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/spf13/cobra"
)

// NewRootCommand builds `azd preview`.
func NewRootCommand() *cobra.Command {
	// `Name` must agree with `namespace` in extension.yaml — azd invokes the
	// binary with the subcommand arguments only.
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

	rootCmd.AddCommand(newUpCommand(&extCtx.OutputFormat))
	rootCmd.AddCommand(newDownCommand(&extCtx.OutputFormat))
	rootCmd.AddCommand(newURLCommand(&extCtx.OutputFormat))
	rootCmd.AddCommand(newStatusCommand(&extCtx.OutputFormat))
	rootCmd.AddCommand(newPromoteCommand(&extCtx.OutputFormat))
	rootCmd.AddCommand(newVersionCommand(&extCtx.OutputFormat))
	rootCmd.AddCommand(newMetadataCommand(rootCmd))

	return rootCmd
}
