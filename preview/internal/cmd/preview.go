package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/spf13/cobra"

	"github.com/obtai/azd-extensions/preview/internal/preview"
)

// session is what every command does before it can do anything: parse the
// pull request, connect to azd, assemble the target. With `--output json`
// the progress goes to stderr so stdout is one JSON object and nothing else.
type session struct {
	ctx    context.Context
	target *preview.Target
	json   bool
	close  func()
}

func open(cmd *cobra.Command, outputFormat *string) (*session, error) {
	asJSON := *outputFormat == "json"
	var log io.Writer = os.Stdout
	if asJSON {
		log = os.Stderr
	}

	ctx := azdext.WithAccessToken(cmd.Context())
	client, err := azdext.NewAzdClient()
	if err != nil {
		return nil, err
	}

	target, err := newTarget(ctx, client, log)
	if err != nil {
		client.Close()
		return nil, err
	}
	return &session{ctx: ctx, target: target, json: asJSON, close: func() { client.Close() }}, nil
}

func (s *session) emit(value any) error {
	if !s.json {
		return nil
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func newUpCommand(outputFormat *string) *cobra.Command {
	var sha, ref, label string

	command := &cobra.Command{
		Use:   "up <pr>",
		Short: "Create or update the preview environment for a pull request.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pr, err := pullRequest(args[0])
			if err != nil {
				return err
			}
			session, err := open(cmd, outputFormat)
			if err != nil {
				return err
			}
			defer session.close()

			result, err := session.target.Create(
				session.ctx, pr, valueOr(sha, "BUILD_SHA"), valueOr(ref, "BUILD_REF"), label)
			if err != nil {
				return err
			}

			if path := os.Getenv("GITHUB_OUTPUT"); path != "" {
				lines := []string{"label=" + result.Label}
				if len(result.Services) > 0 {
					lines = append(lines, "url="+result.Services[0].URL)
				}
				if len(result.Services) > 1 {
					for _, service := range result.Services {
						lines = append(lines, "url_"+service.Name+"="+service.URL)
					}
				}
				if err := appendLines(path, lines...); err != nil {
					return err
				}
			}
			return session.emit(result)
		},
	}

	command.Flags().StringVar(&sha, "sha", "", "Commit this preview is built from. Defaults to $BUILD_SHA.")
	command.Flags().StringVar(&ref, "ref", "", "Branch this preview is built from. Defaults to $BUILD_REF.")
	command.Flags().StringVar(&label, "label", "",
		"Revision label to use, overriding the pool and the pr-<n> default.")
	return command
}

func newDownCommand(outputFormat *string) *cobra.Command {
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
			session, err := open(cmd, outputFormat)
			if err != nil {
				return err
			}
			defer session.close()

			result, err := session.target.Destroy(session.ctx, pr)
			if err != nil {
				return err
			}
			return session.emit(result)
		},
	}
}

func newURLCommand(outputFormat *string) *cobra.Command {
	var service string

	command := &cobra.Command{
		Use:   "url <pr>",
		Short: "Print a pull request's preview URL.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pr, err := pullRequest(args[0])
			if err != nil {
				return err
			}
			session, err := open(cmd, outputFormat)
			if err != nil {
				return err
			}
			defer session.close()

			// Derived when the label is pr-<n>, so this answers whether or not
			// the preview exists. With a label pool the label is a fact about
			// the app and has to be looked up, so a pull request with no
			// preview gets an error rather than a URL to nowhere.
			names, err := session.target.Lookup(session.ctx, pr)
			if err != nil {
				return err
			}
			services, err := pick(names.Services, service)
			if err != nil {
				return err
			}

			if session.json {
				urls := map[string]string{}
				for _, entry := range services {
					urls[entry.Name] = entry.URL
				}
				return session.emit(struct {
					PR    int               `json:"pr"`
					Label string            `json:"label"`
					URLs  map[string]string `json:"urls"`
				}{pr, names.Label, urls})
			}
			for _, entry := range services {
				if len(names.Services) == 1 || service != "" {
					fmt.Println(entry.URL)
				} else {
					fmt.Printf("%s=%s\n", entry.Name, entry.URL)
				}
			}
			return nil
		},
	}

	command.Flags().StringVar(&service, "service", "", "Print one service's URL.")
	return command
}

func newStatusCommand(outputFormat *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status <pr>",
		Short: "Report what exists for a pull request's preview.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pr, err := pullRequest(args[0])
			if err != nil {
				return err
			}
			session, err := open(cmd, outputFormat)
			if err != nil {
				return err
			}
			defer session.close()

			result, err := session.target.Status(session.ctx, pr)
			if err != nil {
				return err
			}
			if err := session.emit(result); err != nil {
				return err
			}
			if !session.json {
				printStatus(result)
			}
			if !result.Exists {
				return fmt.Errorf("no preview for pull request %d", pr)
			}
			return nil
		},
	}
}

func printStatus(result preview.StatusResult) {
	fmt.Printf("label: %s\n", orNone(result.Label))
	fmt.Printf("database: %s (%s)\n", result.Database.Name, tristate(result.Database.Exists, "exists", "missing", "unknown"))
	for _, service := range result.Services {
		fmt.Printf("%s:\n", service.Name)
		fmt.Printf("  app: %s\n", service.App)
		fmt.Printf("  revision: %s\n", orNone(service.Revision))
		fmt.Printf("  url: %s\n", orNone(service.URL))
		fmt.Printf("  healthy: %s\n", tristate(service.Healthy, "yes", "no", "-"))
	}
}

func newPromoteCommand(outputFormat *string) *cobra.Command {
	var service string

	command := &cobra.Command{
		Use:   "promote <pr>",
		Short: "Move a pull request's preview revision to 100% of traffic on the app it previews.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pr, err := pullRequest(args[0])
			if err != nil {
				return err
			}
			session, err := open(cmd, outputFormat)
			if err != nil {
				return err
			}
			defer session.close()

			result, err := session.target.Promote(session.ctx, pr, service)
			if err != nil {
				return err
			}
			return session.emit(result)
		},
	}

	command.Flags().StringVar(&service, "service", "", "Promote one service only.")
	return command
}

func pick(services []preview.ServiceNames, name string) ([]preview.ServiceNames, error) {
	if name == "" {
		return services, nil
	}
	for _, service := range services {
		if service.Name == name {
			return []preview.ServiceNames{service}, nil
		}
	}
	return nil, fmt.Errorf("no service named %q", name)
}

func orNone(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func tristate(value *bool, yes, no, unknown string) string {
	switch {
	case value == nil:
		return unknown
	case *value:
		return yes
	default:
		return no
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

func appendLines(path string, lines ...string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, line := range lines {
		if _, err := file.WriteString(line + "\n"); err != nil {
			return err
		}
	}
	return nil
}
