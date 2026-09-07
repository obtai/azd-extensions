// Package preview creates and destroys per-pull-request preview environments.
//
// Each pull request gets a zero-traffic REVISION of a container app, labelled
// `pr-<n>` and served at its label's own FQDN,
// https://<app>---pr-<n>.<environment default domain>. It shares everything that
// app has: the Container Apps environment, the ingress and its certificate, the
// identity, the registry, the secret references, the probes.
//
// This was an app per pull request, because a first attempt at revisions used a
// deterministic revision SUFFIX and revisions are immutable: the second push
// found an identical template, created nothing, and returned success while
// serving the first build forever. A LABEL fixes that properly. The label is the
// name that must stay stable, so it carries the URL; the suffix is the name that
// must change every push, so it carries the commit. The first attempt made one
// name do both jobs, which is why it could not work.
//
// WHICH APP. `previewApp` in preview.yaml names it, and the choice matters more
// than it looks, because a revision is minted from the APP's template — writing
// a preview necessarily writes APP_ENV=preview and the pull request's database
// name onto whatever app hosts it.
//
//   - A DEDICATED app, declared in the repository's own infrastructure. Nothing
//     else deploys there, so that pollution is harmless: the only other writer is
//     the infrastructure itself, and everything a preview changes, the next
//     preview changes again. No restore, and the app is never anything a person
//     is looking at.
//
//   - THE SERVICE APP itself, when a repository has nowhere else to put previews.
//     Then the pollution is dangerous — a release that patches only the image,
//     which is what `azd deploy` does, would inherit a pull request's database —
//     so the template is put back immediately afterwards, and the consuming
//     repository is expected to carry an independent check before it releases.
//     The restore mints a revision of its own every push, because Container Apps
//     only declines to mint when the template is unchanged from the app's
//     CURRENT one, which at that moment is the preview's.
//
// The first is preferred, and is the reason `previewApp` exists. The second is
// what happens when it is unset.
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

// A revision label may not contain two consecutive dashes and is capped at 64
// characters. A revision name — <app>--<suffix> — is capped at 64 too.
var labelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Target is everything a preview needs to know about where it is going. Every
// field comes from a deployment output.
type Target struct {
	Clients *azure.Clients
	Config  *config.Config
	Project string
	// ServiceApp is what `azd deploy` releases — the app a repository's own
	// pipeline writes to.
	ServiceApp string
	// PreviewApp is what preview revisions are added to. Equal to ServiceApp
	// unless preview.yaml names another, which is the shape to prefer.
	PreviewApp string
	Domain     string
	Login      string
	Server     string
	Vault      string
	Outputs    map[string]string
}

// dedicated reports whether previews have an app to themselves. When they do,
// nothing else deploys there and the app template needs no restoring.
func (t *Target) dedicated() bool { return t.PreviewApp != t.ServiceApp }

func (t *Target) vars(pr int, names Names) config.Vars {
	return config.Vars{
		PR:       pr,
		URL:      names.URL,
		Database: names.Database,
		Label:    names.Label,
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

// Names are everything derived from a pull request number. Deliberately free of
// the commit: `azd preview url` answers before a build exists, and the URL is a
// label, which does not move.
type Names struct {
	App          string
	Label        string
	URL          string
	Database     string
	TagPrefix    string
	SuffixPrefix string
}

func (t *Target) Names(pr int) Names {
	label := fmt.Sprintf("pr-%d", pr)
	return Names{
		App:   t.PreviewApp,
		Label: label,
		// Three dashes: a label's FQDN, not a revision's, which takes two.
		URL:          fmt.Sprintf("https://%s---%s.%s", t.PreviewApp, label, t.Domain),
		Database:     fmt.Sprintf("%s_pr_%d", t.Project, pr),
		TagPrefix:    fmt.Sprintf("pr-%d-", pr),
		SuffixPrefix: fmt.Sprintf("pr%d-", pr),
	}
}

// revisionName is what Container Apps calls the revision a suffix produces.
func revisionName(app, suffix string) string { return app + "--" + suffix }

// Create builds this push's image, migrates the pull request's database, and
// points the pull request's label at a new revision serving it.
func (t *Target) Create(ctx context.Context, pr int, sha, ref string) (Names, error) {
	names := t.Names(pr)

	if len(names.Label) > 64 || strings.Contains(names.Label, "--") ||
		!labelPattern.MatchString(names.Label) {
		return names, fmt.Errorf("invalid revision label %q", names.Label)
	}

	// The suffix must differ per push: Container Apps mints a revision only when
	// the template changes, and an unchanged tag string is an unchanged template
	// whatever digest it now points at. This is the failure the package comment
	// describes, and the stamp is what prevents it.
	stamp := time.Now().UTC().Format("20060102150405")
	if sha != "" {
		stamp = sha[:min(7, len(sha))]
	}
	suffix := names.SuffixPrefix + stamp
	revision := revisionName(names.App, suffix)
	if len(revision) > 64 {
		return names, fmt.Errorf("revision name %q is over 64 characters", revision)
	}

	repositoryTag := fmt.Sprintf("%s:%s%s", t.Project, names.TagPrefix, stamp)
	image := fmt.Sprintf("%s/%s", t.Login, repositoryTag)

	fmt.Printf("==> Building %s\n", image)
	if err := t.Clients.BuildImage(ctx, t.Login, image, t.Config.Dockerfile, ".", map[string]string{
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

	app, err := t.app(ctx, names.App)
	if err != nil {
		return names, err
	}
	if app == nil {
		return names, fmt.Errorf("no app %s to add a preview revision to", names.App)
	}
	if err := multipleRevisionMode(app); err != nil {
		return names, err
	}

	// Where the preview inherits from.
	//
	// A dedicated app: its own template, which its infrastructure owns and
	// re-asserts. It may be carrying the last preview's values, and that is fine
	// — everything a preview changes, a preview sets.
	//
	// The service app: the LIVE revision's template, not the app's. The app's is
	// shared mutable state a concurrent preview may have mid-restore, while the
	// live revision is exactly what is being served.
	base := app.Properties.Template
	live := ""
	if !t.dedicated() {
		live, base, err = t.liveRevision(ctx, app)
		if err != nil {
			return names, err
		}
	}

	fmt.Printf("==> Creating %s\n", revision)
	overrides := config.Expand(t.Config.Env, t.vars(pr, names))
	app.Properties.Template = previewTemplate(base, image, overrides, suffix)
	if err := t.put(ctx, names.App, *app); err != nil {
		return names, err
	}

	// The revision has to exist before a label can point at it.
	//
	// On the service app the restore rides along in this same PUT, so the app
	// spends one round trip holding the preview's environment rather than two —
	// and mints a revision of its own doing it. A dedicated app is left as it is.
	if t.dedicated() {
		fmt.Printf("==> Labelling %s\n", names.Label)
	} else {
		fmt.Printf("==> Labelling %s and restoring %s\n", names.Label, names.App)
	}
	app, err = t.app(ctx, names.App)
	if err != nil {
		return names, err
	}
	if !t.dedicated() {
		app.Properties.Template = forRestore(base)
	}
	app.Properties.Configuration.Ingress.Traffic = withLabel(
		app.Properties.Configuration.Ingress.Traffic, names.Label, revision)
	if err := t.put(ctx, names.App, *app); err != nil {
		return names, err
	}

	if err := t.verify(ctx, names.App, revision, image, live); err != nil {
		return names, err
	}

	if err := azure.WaitHealthy(ctx, t.Clients, names.App, revision); err != nil {
		return names, err
	}

	// The pull request comment goes up the moment this returns. Not fatal on
	// timeout: a cold revision that is slow to warm is worth a note, not a
	// failure.
	fmt.Printf("==> Waiting for %s\n", names.URL)
	if !azure.Until(ctx, serveTimeout, serveInterval, func(ctx context.Context) bool {
		return serving(ctx, names.URL)
	}) {
		fmt.Println("    still starting — it may need another moment")
	}

	fmt.Printf("==> Preview live at %s\n", names.URL)
	return names, nil
}

// verify proves the revision exists and serves this push's image, and — when the
// preview shares the service app — that the restore actually restored. Container
// Apps declines to mint a revision whose template matches an existing one, so a
// successful PUT is not evidence.
//
// `live` is empty for a dedicated app, where there is nothing to restore and so
// nothing to check.
func (t *Target) verify(ctx context.Context, app, revision, image, live string) error {
	response, err := t.Clients.Revisions.GetRevision(ctx, t.Clients.ResourceGroup, app, revision, nil)
	if err != nil {
		if azure.NotFound(err) {
			return fmt.Errorf("%s was not created — the template matched an existing revision", revision)
		}
		return err
	}
	if got := templateImage(response.Properties.Template); got != image {
		return fmt.Errorf("%s is serving %q, expected %q", revision, got, image)
	}
	if live == "" {
		return nil
	}

	// The hazard, checked rather than assumed: the service app must be back on
	// the live revision's template, or the next release inherits this preview's
	// database.
	current, err := t.app(ctx, app)
	if err != nil {
		return err
	}
	liveResponse, err := t.Clients.Revisions.GetRevision(ctx, t.Clients.ResourceGroup, app, live, nil)
	if err != nil {
		return err
	}
	want := templateImage(liveResponse.Properties.Template)
	if got := templateImage(current.Properties.Template); got != want {
		return fmt.Errorf(
			"%s is left holding %q rather than %s's %q — restore it before releasing",
			app, got, live, want)
	}
	return nil
}

// Destroy removes everything a pull request created.
func (t *Target) Destroy(ctx context.Context, pr int) error {
	names := t.Names(pr)

	fmt.Println("==> Label")
	app, err := t.app(ctx, names.App)
	if err != nil {
		return err
	}
	if app == nil {
		fmt.Printf("    no %s\n", names.App)
	} else if traffic := app.Properties.Configuration.Ingress.Traffic; hasLabel(traffic, names.Label) {
		app.Properties.Configuration.Ingress.Traffic = withoutLabel(traffic, names.Label)
		if err := t.put(ctx, names.App, *app); err != nil {
			return err
		}
		fmt.Printf("    removed %s\n", names.Label)
	} else {
		fmt.Printf("    no %s\n", names.Label)
	}

	// Deactivating stops the replicas and the billing. It does NOT free the
	// revision slot: an app holds 100 revisions, active and inactive, and the
	// oldest are purged past that. maxInactiveRevisions on the app is what stops
	// pull request churn evicting rollback history.
	fmt.Println("==> Revisions")
	revisions, err := t.revisionsWithPrefix(ctx, names.App, revisionName(names.App, names.SuffixPrefix))
	if err != nil {
		return err
	}
	for _, revision := range revisions {
		if _, err := t.Clients.Revisions.DeactivateRevision(
			ctx, t.Clients.ResourceGroup, names.App, revision, nil); err != nil {
			return err
		}
		fmt.Printf("    deactivated %s\n", revision)
	}
	if len(revisions) == 0 {
		fmt.Printf("    none matching %s*\n", revisionName(names.App, names.SuffixPrefix))
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

// multipleRevisionMode refuses to touch an app that cannot hold a preview. In
// single-revision mode a new revision takes all the traffic, so getting this
// wrong would put a pull request on the front door.
func multipleRevisionMode(app *armappcontainers.ContainerApp) error {
	if app.Properties == nil || app.Properties.Configuration == nil ||
		app.Properties.Configuration.Ingress == nil {
		return fmt.Errorf("%s has no ingress", *app.Name)
	}
	mode := app.Properties.Configuration.ActiveRevisionsMode
	if mode == nil || *mode != armappcontainers.ActiveRevisionsModeMultiple {
		return fmt.Errorf(
			"%s is not in multiple-revision mode — a preview revision would take production's traffic",
			*app.Name)
	}
	return nil
}

// liveRevision is the revision holding the live label, and its template. Falls
// back to the latest revision on an app whose label has never been assigned,
// which is the state a freshly provisioned environment is in.
func (t *Target) liveRevision(
	ctx context.Context, app *armappcontainers.ContainerApp,
) (string, *armappcontainers.Template, error) {
	name := ""
	for _, weight := range app.Properties.Configuration.Ingress.Traffic {
		if weight != nil && weight.Label != nil && *weight.Label == t.Config.LiveLabel &&
			weight.RevisionName != nil {
			name = *weight.RevisionName
			break
		}
	}
	if name == "" && app.Properties.LatestRevisionName != nil {
		name = *app.Properties.LatestRevisionName
		fmt.Printf("    no revision labelled %s — falling back to %s\n", t.Config.LiveLabel, name)
	}
	if name == "" {
		return "", nil, fmt.Errorf("%s has no revisions to base a preview on", *app.Name)
	}

	response, err := t.Clients.Revisions.GetRevision(ctx, t.Clients.ResourceGroup, *app.Name, name, nil)
	if err != nil {
		return "", nil, err
	}
	if response.Properties == nil || response.Properties.Template == nil {
		return "", nil, fmt.Errorf("%s has no template", name)
	}

	return name, forRestore(response.Properties.Template), nil
}

// forRestore prepares a fetched revision's template to be written back onto the
// app.
//
// The suffix is CLEARED rather than carried over. Putting it back names a
// revision that already exists, and Container Apps rejects that outright —
// "revision with suffix X already exists" — rather than treating it as the
// no-op it looks like. Without a suffix Container Apps generates one, and the
// restore succeeds at the cost of an inactive revision. See the package comment.
func forRestore(template *armappcontainers.Template) *armappcontainers.Template {
	if template == nil {
		return nil
	}
	restored := *template
	restored.RevisionSuffix = nil
	return &restored
}

func (t *Target) revisionsWithPrefix(ctx context.Context, app, prefix string) ([]string, error) {
	var found []string
	pager := t.Clients.Revisions.NewListRevisionsPager(t.Clients.ResourceGroup, app, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, revision := range page.Value {
			if revision == nil || revision.Name == nil || !strings.HasPrefix(*revision.Name, prefix) {
				continue
			}
			// Deactivating an inactive revision is an error, not a no-op.
			if revision.Properties != nil && revision.Properties.Active != nil &&
				!*revision.Properties.Active {
				continue
			}
			found = append(found, *revision.Name)
		}
	}
	return found, nil
}

// withLabel points a label at a revision at zero weight, leaving every other
// entry — and so the traffic split — exactly as it was.
func withLabel(
	traffic []*armappcontainers.TrafficWeight, label, revision string,
) []*armappcontainers.TrafficWeight {
	kept := withoutLabel(traffic, label)
	return append(kept, &armappcontainers.TrafficWeight{
		RevisionName: to.Ptr(revision),
		Weight:       to.Ptr(int32(0)),
		Label:        to.Ptr(label),
	})
}

func withoutLabel(
	traffic []*armappcontainers.TrafficWeight, label string,
) []*armappcontainers.TrafficWeight {
	kept := make([]*armappcontainers.TrafficWeight, 0, len(traffic))
	for _, weight := range traffic {
		if weight != nil && weight.Label != nil && *weight.Label == label {
			continue
		}
		kept = append(kept, weight)
	}
	return kept
}

func hasLabel(traffic []*armappcontainers.TrafficWeight, label string) bool {
	for _, weight := range traffic {
		if weight != nil && weight.Label != nil && *weight.Label == label {
			return true
		}
	}
	return false
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

func templateImage(template *armappcontainers.Template) string {
	if template == nil || len(template.Containers) == 0 || template.Containers[0] == nil ||
		template.Containers[0].Image == nil {
		return ""
	}
	return *template.Containers[0].Image
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

// previewTemplate derives a preview revision from the template production is
// serving, rather than restating probes, resources and environment in a config
// file that would drift. A revision inherits the app's identity, registries and
// secret references natively, so unlike an app per pull request there is nothing
// to copy but the template itself.
func previewTemplate(
	live *armappcontainers.Template, image string, overrides map[string]string, suffix string,
) *armappcontainers.Template {
	source := live.Containers[0]

	pending := make(map[string]string, len(overrides))
	for key, value := range overrides {
		pending[key] = value
	}

	env := make([]*armappcontainers.EnvironmentVar, 0, len(source.Env)+len(pending))
	for _, entry := range source.Env {
		if entry != nil && entry.Name != nil {
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

	return &armappcontainers.Template{
		RevisionSuffix: to.Ptr(suffix),
		Containers: []*armappcontainers.Container{{
			Name:      source.Name,
			Image:     to.Ptr(image),
			Resources: source.Resources,
			Probes:    source.Probes,
			Env:       env,
		}},
		// Scales to zero, so an idle preview costs nothing.
		Scale: &armappcontainers.Scale{
			MinReplicas: to.Ptr(int32(0)),
			MaxReplicas: to.Ptr(int32(1)),
		},
	}
}
