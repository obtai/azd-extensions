package cmd

import (
	"fmt"
	"os"
	"strconv"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/spf13/cobra"
)

// newUpCommand is the whole developer-facing surface:
//
//	azd preview up 123        create or update the preview for pull request 123
//	azd preview down 123      tear it down
//	azd preview url 123       print its URL and nothing else
func newUpCommand() *cobra.Command {
	var sha, ref string

	command := &cobra.Command{
		Use:   "up <pr>",
		Short: "Create or update the preview environment for a pull request.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pr, err := pullRequest(args[0])
			if err != nil {
				return err
			}

			ctx := azdext.WithAccessToken(cmd.Context())
			client, err := azdext.NewAzdClient()
			if err != nil {
				return err
			}
			defer client.Close()

			target, err := newTarget(ctx, client)
			if err != nil {
				return err
			}

			names, err := target.Create(ctx, pr, valueOr(sha, "BUILD_SHA"), valueOr(ref, "BUILD_REF"))
			if err != nil {
				return err
			}

			// So a workflow can put it in a comment without asking again.
			if path := os.Getenv("GITHUB_OUTPUT"); path != "" {
				return appendLine(path, "url="+names.URL)
			}
			return nil
		},
	}

	command.Flags().StringVar(&sha, "sha", "", "Commit this preview is built from. Defaults to $BUILD_SHA.")
	command.Flags().StringVar(&ref, "ref", "", "Branch this preview is built from. Defaults to $BUILD_REF.")
	return command
}

func newDownCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "down <pr>",
		Short:   "Delete a pull request's preview environment, its database and its images.",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pr, err := pullRequest(args[0])
			if err != nil {
				return err
			}

			ctx := azdext.WithAccessToken(cmd.Context())
			client, err := azdext.NewAzdClient()
			if err != nil {
				return err
			}
			defer client.Close()

			target, err := newTarget(ctx, client)
			if err != nil {
				return err
			}
			return target.Destroy(ctx, pr)
		},
	}
}

func newURLCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "url <pr>",
		Short: "Print a pull request's preview URL.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pr, err := pullRequest(args[0])
			if err != nil {
				return err
			}

			ctx := azdext.WithAccessToken(cmd.Context())
			client, err := azdext.NewAzdClient()
			if err != nil {
				return err
			}
			defer client.Close()

			target, err := newTarget(ctx, client)
			if err != nil {
				return err
			}

			// Derived, not looked up: the app name is stable for the life of
			// the pull request, so this answers whether or not the preview
			// exists — which is what a comment on a failed build needs.
			fmt.Println(target.Names(pr).URL)
			return nil
		},
	}
}

func pullRequest(raw string) (int, error) {
	pr, err := strconv.Atoi(raw)
	if err != nil || pr <= 0 {
		return 0, fmt.Errorf("pull request must be a positive number, got %q", raw)
	}
	return pr, nil
}

func valueOr(flag, envVar string) string {
	if flag != "" {
		return flag
	}
	return os.Getenv(envVar)
}

func appendLine(path, line string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(line + "\n")
	return err
}
