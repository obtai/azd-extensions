// Package preview creates and destroys per-pull-request preview environments.
//
// Each pull request gets its own container app in an existing environment,
// served at https://ca-<project>-pr-<n>.<environment default domain> and
// sharing everything expensive: the Container Apps environment, the database
// server, the registry, the managed identity, the Key Vault.
//
// An app rather than a zero-traffic revision of the primary app, because
// revisions are immutable: the second push to a pull request would find an
// identical template, create nothing, and return success while serving the
// first build forever. An app keeps the URL stable while the image tag changes
// per push.
package preview

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"

	"github.com/obtai/azd-extensions/preview/internal/azure"
	"github.com/obtai/azd-extensions/preview/internal/config"
)

const (
	serveTimeout  = 3 * time.Minute
	serveInterval = 5 * time.Second
)

var appNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)

// Target is everything a preview needs to know about where it is going. Every
// field comes from a deployment output.
type Target struct {
	Clients   *azure.Clients
	Config    *config.Config
	Project   string
	SourceApp string
	Domain    string
	Registry  string
	Login     string
	Server    string
	Vault     string
	Outputs   map[string]string
}

func (t *Target) vars(pr int, names Names) config.Vars {
	return config.Vars{
		PR:       pr,
		URL:      names.URL,
		Database: names.Database,
		Outputs:  t.Outputs,
		Secret:   t.secret,
	}
}

func (t *Target) secret(name string) (string, error) {
	if t.Vault == "" {
		return "", fmt.Errorf("no Key Vault in the deployment outputs")
	}
	client, err := t.Clients.Secrets(t.Vault)
	if err != nil {
		return "", err
	}
	response, err := client.GetSecret(context.Background(), name, "", nil)
	if err != nil {
		return "", err
	}
	if response.Value == nil {
		return "", fmt.Errorf("%s in %s has no value", name, t.Vault)
	}
	return *response.Value, nil
}

// Names are everything derived from a pull request number.
type Names struct {
	App       string
	URL       string
	Database  string
	TagPrefix string
}

func (t *Target) Names(pr int) Names {
	app := fmt.Sprintf("ca-%s-pr-%d", t.Project, pr)
	return Names{
		App:       app,
		URL:       fmt.Sprintf("https://%s.%s", app, t.Domain),
		Database:  fmt.Sprintf("%s_pr_%d", t.Project, pr),
		TagPrefix: fmt.Sprintf("pr-%d-", pr),
	}
}

// Create builds this push's image, migrates the pull request's database, and
// creates or updates its app.
func (t *Target) Create(ctx context.Context, pr int, sha, ref string) (Names, error) {
	names := t.Names(pr)

	// Container app names are capped at 32 characters.
	if len(names.App) > 32 || !appNamePattern.MatchString(names.App) {
		return names, fmt.Errorf("invalid container app name %q", names.App)
	}

	// The tag must differ per push: Container Apps creates a revision only when
	// the template changes, and an unchanged tag string is an unchanged
	// template whatever digest it now points at.
	stamp := time.Now().UTC().Format("20060102150405")
	if sha != "" {
		stamp = sha[:min(7, len(sha))]
	}
	repositoryTag := fmt.Sprintf("%s:%s%s", t.Project, names.TagPrefix, stamp)
	image := fmt.Sprintf("%s/%s", t.Login, repositoryTag)

	fmt.Printf("==> Building %s\n", image)
	if err := t.Clients.BuildImage(ctx, t.Registry, repositoryTag, t.Config.Dockerfile, ".", map[string]string{
		"BUILD_SHA": sha,
		"BUILD_REF": ref,
		"BUILD_PR":  fmt.Sprint(pr),
	}); err != nil {
		return names, err
	}

	if t.Config.Provision != "" {
		fmt.Printf("==> Provisioning %s\n", names.Database)
		if err := t.provision(ctx, pr, names); err != nil {
			return names, err
		}
	}

	existing, err := t.app(ctx, names.App)
	if err != nil {
		return names, err
	}

	if existing != nil {
		// Read, mutate and write the whole app: a partial template drops the
		// probes, environment and resources alongside the image.
		fmt.Printf("==> Updating %s\n", names.App)
		if existing.Properties == nil || existing.Properties.Template == nil ||
			len(existing.Properties.Template.Containers) == 0 {
			return names, fmt.Errorf("%s has no containers", names.App)
		}
		existing.Properties.Template.Containers[0].Image = to.Ptr(image)
		if err := t.put(ctx, names.App, *existing); err != nil {
			return names, err
		}
	} else {
		fmt.Printf("==> Creating %s\n", names.App)
		source, err := t.app(ctx, t.SourceApp)
		if err != nil {
			return names, err
		}
		if source == nil {
			return names, fmt.Errorf("no app %s to clone a preview from", t.SourceApp)
		}
		overrides := config.Expand(t.Config.Env, t.vars(pr, names))
		if err := t.put(ctx, names.App, clone(*source, image, overrides)); err != nil {
			return names, err
		}
	}

	// Container Apps silently declines to create a revision whose template
	// matches the running one, so a successful call is not evidence.
	running, err := t.app(ctx, names.App)
	if err != nil {
		return names, err
	}
	if got := runningImage(running); got != image {
		return names, fmt.Errorf("%s is serving %q, expected %q", names.App, got, image)
	}

	// The pull request comment goes up the moment this returns. Not fatal on
	// timeout: a cold app that is slow to warm is worth a note, not a failure.
	fmt.Printf("==> Waiting for %s\n", names.URL)
	if !azure.Until(ctx, serveTimeout, serveInterval, func(ctx context.Context) bool {
		return serving(ctx, names.URL)
	}) {
		fmt.Println("    still starting — it may need another moment")
	}

	fmt.Printf("==> Preview live at %s\n", names.URL)
	return names, nil
}

// Destroy removes everything a pull request created.
func (t *Target) Destroy(ctx context.Context, pr int) error {
	names := t.Names(pr)

	fmt.Println("==> App")
	existing, err := t.app(ctx, names.App)
	if err != nil {
		return err
	}
	if existing == nil {
		fmt.Printf("    no %s\n", names.App)
	} else {
		poller, err := t.Clients.ContainerApps.BeginDelete(ctx, t.Clients.ResourceGroup, names.App, nil)
		if err != nil {
			return err
		}
		if _, err := poller.PollUntilDone(ctx, nil); err != nil {
			return err
		}
		fmt.Printf("    deleted %s\n", names.App)
	}

	fmt.Println("==> Database")
	if t.Server == "" {
		fmt.Println("    no database server in the deployment outputs")
	} else if err := t.dropDatabase(ctx, names.Database); err != nil {
		return err
	}

	// Found by prefix: the pull request pushed one tag per commit.
	fmt.Println("==> Image tags")
	tags, err := t.Clients.TagsWithPrefix(ctx, t.Login, t.Project, names.TagPrefix)
	if err != nil {
		return err
	}
	for _, tag := range tags {
		if err := t.Clients.DeleteTag(ctx, t.Login, t.Project, tag); err != nil {
			return err
		}
		fmt.Printf("    untagged %s:%s\n", t.Project, tag)
	}
	if len(tags) == 0 {
		fmt.Printf("    none matching %s*\n", names.TagPrefix)
	}
	return nil
}

func (t *Target) app(ctx context.Context, name string) (*armappcontainers.ContainerApp, error) {
	response, err := t.Clients.ContainerApps.Get(ctx, t.Clients.ResourceGroup, name, nil)
	if err != nil {
		if azure.NotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &response.ContainerApp, nil
}

func (t *Target) put(ctx context.Context, name string, app armappcontainers.ContainerApp) error {
	poller, err := t.Clients.ContainerApps.BeginCreateOrUpdate(ctx, t.Clients.ResourceGroup, name, app, nil)
	if err != nil {
		return err
	}
	_, err = poller.PollUntilDone(ctx, nil)
	return err
}

func (t *Target) provision(ctx context.Context, pr int, names Names) error {
	return t.run(ctx, pr, names, config.Expand(t.Config.Env, t.vars(pr, names)))
}

// run executes the repository's provision command with the environment a
// migration needs.
func (t *Target) run(ctx context.Context, pr int, names Names, env map[string]string) error {
	// Every deployment output, then provisionEnv, then the stage-specific
	// values, each layer overriding the one before. The outputs go in wholesale
	// because azd hands an extension its environment over gRPC, so a subprocess
	// inherits none of them.
	resolved := map[string]string{}
	for key, value := range t.Outputs {
		resolved[key] = value
	}

	fromConfig, err := config.ExpandFull(t.Config.ProvisionEnv, t.vars(pr, names))
	if err != nil {
		return err
	}
	for key, value := range fromConfig {
		resolved[key] = value
	}

	for key, value := range env {
		resolved[key] = value
	}

	command := exec.CommandContext(ctx, "sh", "-c", t.Config.Provision)

	// Streamed and captured: azd's progress display swallows a handler's
	// stdout and reports only "exit status 1".
	var captured bytes.Buffer
	command.Stdout = io.MultiWriter(os.Stdout, &captured)
	command.Stderr = io.MultiWriter(os.Stderr, &captured)

	command.Env = os.Environ()
	for key, value := range resolved {
		command.Env = append(command.Env, key+"="+value)
	}

	if err := command.Run(); err != nil {
		return fmt.Errorf("%s: %w\n%s", t.Config.Provision, err, tail(captured.String(), 30))
	}
	return nil
}

// tail returns the last n lines.
func tail(output string, n int) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (t *Target) dropDatabase(ctx context.Context, name string) error {
	_, err := t.Clients.Databases.Get(ctx, t.Clients.ResourceGroup, t.Server, name, nil)
	if err != nil {
		if azure.NotFound(err) {
			fmt.Printf("    no %s\n", name)
			return nil
		}
		return err
	}

	poller, err := t.Clients.Databases.BeginDelete(ctx, t.Clients.ResourceGroup, t.Server, name, nil)
	if err != nil {
		return err
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return err
	}
	fmt.Printf("    dropped %s\n", name)
	return nil
}

func runningImage(app *armappcontainers.ContainerApp) string {
	if app == nil || app.Properties == nil || app.Properties.Template == nil ||
		len(app.Properties.Template.Containers) == 0 ||
		app.Properties.Template.Containers[0].Image == nil {
		return ""
	}
	return *app.Properties.Template.Containers[0].Image
}

func serving(ctx context.Context, url string) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/health", nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}

// clone derives a preview app from the app it previews, rather than restating
// its identity, registry, secret references and probes in a config file that
// would drift.
func clone(source armappcontainers.ContainerApp, image string, overrides map[string]string) armappcontainers.ContainerApp {
	container := source.Properties.Template.Containers[0]

	pending := make(map[string]string, len(overrides))
	for key, value := range overrides {
		pending[key] = value
	}

	env := make([]*armappcontainers.EnvironmentVar, 0, len(container.Env)+len(pending))
	for _, entry := range container.Env {
		if entry.Name != nil {
			if value, ok := pending[*entry.Name]; ok {
				env = append(env, &armappcontainers.EnvironmentVar{
					Name:  entry.Name,
					Value: to.Ptr(value),
				})
				delete(pending, *entry.Name)
				continue
			}
		}
		env = append(env, entry)
	}
	for key, value := range pending {
		env = append(env, &armappcontainers.EnvironmentVar{Name: to.Ptr(key), Value: to.Ptr(value)})
	}

	// Only the identity names carry over; the ids are read-only on create.
	var identity *armappcontainers.ManagedServiceIdentity
	if source.Identity != nil {
		assigned := map[string]*armappcontainers.UserAssignedIdentity{}
		for id := range source.Identity.UserAssignedIdentities {
			assigned[id] = &armappcontainers.UserAssignedIdentity{}
		}
		identity = &armappcontainers.ManagedServiceIdentity{
			Type:                   source.Identity.Type,
			UserAssignedIdentities: assigned,
		}
	}

	ingress := source.Properties.Configuration.Ingress

	return armappcontainers.ContainerApp{
		Location: source.Location,
		Identity: identity,
		Properties: &armappcontainers.ContainerAppProperties{
			EnvironmentID:       source.Properties.EnvironmentID,
			WorkloadProfileName: source.Properties.WorkloadProfileName,
			Configuration: &armappcontainers.Configuration{
				// Single: a preview has one thing to serve.
				ActiveRevisionsMode: to.Ptr(armappcontainers.ActiveRevisionsModeSingle),
				Ingress: &armappcontainers.Ingress{
					External:      ingress.External,
					TargetPort:    ingress.TargetPort,
					Transport:     ingress.Transport,
					AllowInsecure: ingress.AllowInsecure,
				},
				Registries: source.Properties.Configuration.Registries,
				Secrets:    source.Properties.Configuration.Secrets,
			},
			Template: &armappcontainers.Template{
				Containers: []*armappcontainers.Container{{
					Name:      container.Name,
					Image:     to.Ptr(image),
					Resources: container.Resources,
					Probes:    container.Probes,
					Env:       env,
				}},
				// Scales to zero, so an idle preview costs nothing.
				Scale: &armappcontainers.Scale{
					MinReplicas: to.Ptr(int32(0)),
					MaxReplicas: to.Ptr(int32(1)),
				},
			},
		},
	}
}
