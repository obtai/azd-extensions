// Package preview creates and destroys per-pull-request preview environments.
//
// Each pull request gets a zero-traffic REVISION of a container app, carrying a
// label and served at the label's own FQDN,
// https://<app>---<label>.<environment default domain>. It shares everything
// that app has: the Container Apps environment, the ingress and its
// certificate, the identity, the registry, the secret references, the probes.
//
// This was an app per pull request, because a first attempt at revisions used a
// deterministic revision SUFFIX and revisions are immutable: the second push
// found an identical template, created nothing, and returned success while
// serving the first build forever. A LABEL fixes that properly. The label is the
// name that must stay stable, so it carries the URL; the suffix is the name that
// must change every push, so it carries the commit. The first attempt made one
// name do both jobs, which is why it could not work.
//
// WHICH LABEL. `pr-<n>` by default. A repository that lists `labels` in
// preview.yaml has a POOL instead: a pull request borrows the first free name
// and keeps it until `down`, which is what lets an app that signs people in
// through Entra register its callback URLs once rather than per pull request.
// The label is then a fact about the app, not the number — which is why `url`,
// `down` and `status` look it up by the revision name the label points at.
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
// what happens when it is unset — and what every entry under `services` is,
// because there `app` names the app azd deploys.
package preview

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
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

// Target is everything a preview needs to know about where it is going. Every
// field comes from a deployment output, or from what stands in for one.
type Target struct {
	Clients  *azure.Clients
	Config   *config.Config
	Project  string
	Services []Service
	Domain   string
	Login    string
	Server   string
	Vault    string
	Outputs  map[string]string
	// Log is where progress goes; stdout unless the caller has claimed it.
	Log io.Writer
}

// Service is one container app of the preview, resolved.
type Service struct {
	Name string
	// ServiceApp is what `azd deploy` releases — the app a repository's own
	// pipeline writes to.
	ServiceApp string
	// PreviewApp is what preview revisions are added to. Equal to ServiceApp
	// unless preview.yaml names another, which is the shape to prefer.
	PreviewApp string
	Config     config.Service
}

// dedicated reports whether previews have an app to themselves. When they do,
// nothing else deploys there and the app template needs no restoring.
func (s Service) dedicated() bool { return s.PreviewApp != s.ServiceApp }

func (t *Target) log() io.Writer {
	if t.Log == nil {
		return os.Stdout
	}
	return t.Log
}

func (t *Target) printf(format string, args ...any) {
	fmt.Fprintf(t.log(), format, args...)
}

func (t *Target) pooled() bool { return len(t.Config.Labels) > 0 }

func (t *Target) vars(names Names, url string) config.Vars {
	return config.Vars{
		PR:       names.PR,
		URL:      url,
		Database: names.Database,
		Label:    names.Label,
		URLs:     names.urls(),
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

// Names are everything derived from a pull request number and its label.
// Deliberately free of the commit: the URL is a label, which does not move.
type Names struct {
	PR           int
	Label        string
	Database     string
	TagPrefix    string
	SuffixPrefix string
	Services     []ServiceNames
}

// ServiceNames is one service's share of Names.
type ServiceNames struct {
	Name       string
	App        string
	Repository string
	URL        string
	FQDN       string
	HealthPath string
}

func (n Names) urls() map[string]string {
	urls := make(map[string]string, len(n.Services))
	for _, service := range n.Services {
		urls[service.Name] = service.URL
	}
	return urls
}

// DefaultLabel is the label a pull request gets when there is no pool.
func DefaultLabel(pr int) string { return fmt.Sprintf("pr-%d", pr) }

// Names derives every name from a pull request and the label it holds.
func (t *Target) Names(pr int, label string) Names {
	names := Names{
		PR:           pr,
		Label:        label,
		Database:     t.database(pr, label),
		TagPrefix:    fmt.Sprintf("pr-%d-", pr),
		SuffixPrefix: fmt.Sprintf("pr%d-", pr),
	}
	for _, service := range t.Services {
		// Three dashes: a label's FQDN, not a revision's, which takes two.
		fqdn := fmt.Sprintf("%s---%s.%s", service.PreviewApp, label, t.Domain)
		names.Services = append(names.Services, ServiceNames{
			Name:       service.Name,
			App:        service.PreviewApp,
			Repository: service.Config.Image,
			URL:        "https://" + fqdn,
			FQDN:       fqdn,
			HealthPath: service.Config.HealthPath,
		})
	}
	return names
}

func (t *Target) database(pr int, label string) string {
	if t.Config.Database != nil && t.Config.Database.Name != "" {
		return strings.NewReplacer("${pr}", fmt.Sprint(pr), "${label}", label).
			Replace(t.Config.Database.Name)
	}
	return fmt.Sprintf("%s_pr_%d", t.Project, pr)
}

// revisionName is what Container Apps calls the revision a suffix produces.
func revisionName(app, suffix string) string { return app + "--" + suffix }

// ── Labels ───────────────────────────────────────────────────────────────

// appTraffic is the part of an app the label arithmetic reads.
type appTraffic struct {
	App     string
	Traffic []*armappcontainers.TrafficWeight
}

// heldBy reports whether an entry points at one of this pull request's own
// revisions — the one fact that ties a pooled label to a number.
func heldBy(entry *armappcontainers.TrafficWeight, app, suffixPrefix string) bool {
	return entry != nil && entry.RevisionName != nil &&
		strings.HasPrefix(*entry.RevisionName, revisionName(app, suffixPrefix))
}

// heldLabel is the pool label this pull request already holds on any of its
// apps, or "". Every service borrows the SAME name, so an app whose entry has
// gone missing still answers to the label its sibling holds.
func heldLabel(pool []string, suffixPrefix string, apps []appTraffic) string {
	for _, label := range pool {
		for _, app := range apps {
			for _, entry := range app.Traffic {
				if entry != nil && entry.Label != nil && *entry.Label == label &&
					heldBy(entry, app.App, suffixPrefix) {
					return label
				}
			}
		}
	}
	return ""
}

// freeLabel is the first pool label no app has an entry for, or "".
func freeLabel(pool []string, apps []appTraffic) string {
	for _, label := range pool {
		free := true
		for _, app := range apps {
			if hasLabel(app.Traffic, label) {
				free = false
				break
			}
		}
		if free {
			return label
		}
	}
	return ""
}

// chooseLabel is `up`'s pick: the label the pull request holds, else the first
// free one, else nothing — a full pool is an error, not a queue.
func chooseLabel(pool []string, suffixPrefix string, apps []appTraffic) (string, error) {
	if label := heldLabel(pool, suffixPrefix, apps); label != "" {
		return label, nil
	}
	if label := freeLabel(pool, apps); label != "" {
		return label, nil
	}
	return "", fmt.Errorf("all %d preview labels are in use", len(pool))
}

func (t *Target) trafficOf(apps map[string]*armappcontainers.ContainerApp) []appTraffic {
	var found []appTraffic
	for _, service := range t.Services {
		app := apps[service.PreviewApp]
		if app == nil || app.Properties == nil || app.Properties.Configuration == nil ||
			app.Properties.Configuration.Ingress == nil {
			continue
		}
		found = append(found, appTraffic{
			App:     service.PreviewApp,
			Traffic: app.Properties.Configuration.Ingress.Traffic,
		})
	}
	return found
}

// apps fetches every service's preview app. A missing one is nil.
func (t *Target) apps(ctx context.Context) (map[string]*armappcontainers.ContainerApp, error) {
	apps := map[string]*armappcontainers.ContainerApp{}
	for _, service := range t.Services {
		if _, done := apps[service.PreviewApp]; done {
			continue
		}
		app, err := t.app(ctx, service.PreviewApp)
		if err != nil {
			return nil, err
		}
		apps[service.PreviewApp] = app
	}
	return apps, nil
}

// Label is the label a pull request holds, or "" when it holds none. Without a
// pool the answer is derived; with one it has to be read off the apps.
func (t *Target) Label(ctx context.Context, pr int) (string, error) {
	if !t.pooled() {
		return DefaultLabel(pr), nil
	}
	apps, err := t.apps(ctx)
	if err != nil {
		return "", err
	}
	return heldLabel(t.Config.Labels, t.Names(pr, "").SuffixPrefix, t.trafficOf(apps)), nil
}

// Lookup is `url`'s answer: the names, provided the pull request has a label.
func (t *Target) Lookup(ctx context.Context, pr int) (Names, error) {
	label, err := t.Label(ctx, pr)
	if err != nil {
		return Names{}, err
	}
	if label == "" {
		return Names{}, fmt.Errorf("pull request %d holds no preview label", pr)
	}
	return t.Names(pr, label), nil
}

// ── Up ───────────────────────────────────────────────────────────────────

// Result is what `up` produced.
type Result struct {
	PR       int             `json:"pr"`
	Label    string          `json:"label"`
	Database string          `json:"database"`
	Services []ServiceResult `json:"services"`
}

// ServiceResult is one service's share of a Result.
type ServiceResult struct {
	Name     string `json:"name"`
	App      string `json:"app"`
	URL      string `json:"url"`
	FQDN     string `json:"fqdn"`
	Revision string `json:"revision"`
	Image    string `json:"image"`
}

// Create builds this push's images, prepares the pull request's database, and
// points the pull request's label at a new revision of every service.
func (t *Target) Create(ctx context.Context, pr int, sha, ref, labelOverride string) (Result, error) {
	result := Result{PR: pr}

	apps, err := t.apps(ctx)
	if err != nil {
		return result, err
	}
	for _, service := range t.Services {
		app := apps[service.PreviewApp]
		if app == nil {
			return result, fmt.Errorf("no app %s to add a preview revision to", service.PreviewApp)
		}
		if err := multipleRevisionMode(app); err != nil {
			return result, err
		}
	}

	label := labelOverride
	switch {
	case label != "":
		if err := config.ValidateLabel(label); err != nil {
			return result, err
		}
	case t.pooled():
		label, err = chooseLabel(t.Config.Labels, t.Names(pr, "").SuffixPrefix, t.trafficOf(apps))
		if err != nil {
			return result, err
		}
	default:
		label = DefaultLabel(pr)
		if err := config.ValidateLabel(label); err != nil {
			return result, err
		}
	}
	names := t.Names(pr, label)
	result.Label, result.Database = label, names.Database
	t.printf("==> Label %s\n", label)

	// The suffix must differ per push: Container Apps mints a revision only when
	// the template changes, and an unchanged tag string is an unchanged template
	// whatever digest it now points at. This is the failure the package comment
	// describes, and the stamp is what prevents it.
	stamp := time.Now().UTC().Format("20060102150405")
	if sha != "" {
		stamp = sha[:min(7, len(sha))]
	}
	suffix := names.SuffixPrefix + stamp
	for _, service := range names.Services {
		if revision := revisionName(service.App, suffix); len(revision) > 64 {
			return result, fmt.Errorf("revision name %q is over 64 characters", revision)
		}
	}

	if t.Config.Database != nil && t.Config.Database.Create {
		if t.Server == "" {
			return result, fmt.Errorf(
				"database.create needs a Postgres server — set database.server, " +
					"outputs.postgresServer, or the <PREFIX>_POSTGRES_SERVER_NAME output")
		}
		t.printf("==> Database %s on %s\n", names.Database, t.Server)
		created, err := t.Clients.EnsureDatabase(ctx, t.Server, names.Database)
		if err != nil {
			return result, err
		}
		if created {
			t.printf("    created\n")
		} else {
			t.printf("    exists\n")
		}
	}

	buildArgs := map[string]string{
		"BUILD_SHA": sha,
		"BUILD_REF": ref,
		"BUILD_PR":  fmt.Sprint(pr),
	}
	for i, service := range t.Services {
		image := fmt.Sprintf("%s/%s:%s%s", t.Login, names.Services[i].Repository, names.TagPrefix, stamp)
		result.Services = append(result.Services, ServiceResult{
			Name:     service.Name,
			App:      service.PreviewApp,
			URL:      names.Services[i].URL,
			FQDN:     names.Services[i].FQDN,
			Revision: revisionName(service.PreviewApp, suffix),
			Image:    image,
		})
		t.printf("==> Building %s\n", image)
		if err := t.Clients.BuildImage(ctx, t.Login, image,
			service.Config.Dockerfile, service.Config.Context, buildArgs); err != nil {
			return result, err
		}
	}

	if t.Config.Provision != "" {
		t.printf("==> Provisioning %s\n", names.Database)
		if err := t.provision(ctx, names); err != nil {
			return result, err
		}
	}

	// Mint every service, then gate on all of them. A service that never comes
	// up fails the whole pull request, and takes its siblings' new revisions
	// down with it — half a preview is worse than none.
	var minted []ServiceResult
	for i, service := range t.Services {
		if err := t.mint(ctx, service, names, names.Services[i], result.Services[i], suffix); err != nil {
			t.deactivate(ctx, minted)
			return result, err
		}
		minted = append(minted, result.Services[i])
	}
	for _, service := range minted {
		if err := azure.WaitHealthy(ctx, t.Clients, service.App, service.Revision); err != nil {
			// WaitHealthy has deactivated the one that failed.
			t.deactivate(ctx, minted)
			return result, err
		}
	}

	// Older revisions of this pull request are now unlabelled and idle. An app
	// holds 100 revisions, active and inactive, and purges the oldest past
	// that — a busy pull request must not be what evicts rollback history.
	t.printf("==> Retiring earlier revisions\n")
	for i, service := range t.Services {
		if err := t.retire(ctx, service.PreviewApp, names.Services[i].Repository, names,
			result.Services[i].Revision, names.TagPrefix+stamp); err != nil {
			return result, err
		}
	}

	// The pull request comment goes up the moment this returns. Not fatal on
	// timeout: a cold revision that is slow to warm is worth a note, not a
	// failure.
	for _, service := range names.Services {
		t.printf("==> Waiting for %s%s\n", service.URL, service.HealthPath)
		if !azure.Until(ctx, serveTimeout, serveInterval, func(ctx context.Context) bool {
			return serving(ctx, service.URL+service.HealthPath)
		}) {
			t.printf("    still starting — it may need another moment\n")
		}
	}

	for _, service := range names.Services {
		t.printf("==> Preview live at %s\n", service.URL)
	}
	return result, nil
}

// mint writes one service's preview revision and labels it.
func (t *Target) mint(
	ctx context.Context,
	service Service,
	names Names,
	serviceNames ServiceNames,
	planned ServiceResult,
	suffix string,
) error {
	// Fetched again here rather than before the build: the build takes
	// minutes, and the app is shared state.
	app, err := t.app(ctx, service.PreviewApp)
	if err != nil {
		return err
	}
	if app == nil {
		return fmt.Errorf("no app %s to add a preview revision to", service.PreviewApp)
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
	if !service.dedicated() {
		live, base, err = t.liveRevision(ctx, app)
		if err != nil {
			return err
		}
	}

	t.printf("==> Creating %s\n", planned.Revision)
	overrides, err := config.ExpandFull(service.Config.Env, t.vars(names, serviceNames.URL))
	if err != nil {
		return err
	}
	scaleMin, scaleMax := scaleOf(service.Config.Scale)
	template, err := previewTemplate(base, planned.Image, service.Config.Container, overrides, suffix, scaleMin, scaleMax)
	if err != nil {
		return fmt.Errorf("%s: %w", service.PreviewApp, err)
	}
	app.Properties.Template = template
	if err := t.put(ctx, service.PreviewApp, *app); err != nil {
		return err
	}

	// The revision has to exist before a label can point at it.
	//
	// On the service app the restore rides along in this same PUT, so the app
	// spends one round trip holding the preview's environment rather than two —
	// and mints a revision of its own doing it. A dedicated app is left as it is.
	if service.dedicated() {
		t.printf("==> Labelling %s\n", names.Label)
	} else {
		t.printf("==> Labelling %s and restoring %s\n", names.Label, service.PreviewApp)
	}
	app, err = t.app(ctx, service.PreviewApp)
	if err != nil {
		return err
	}
	if !service.dedicated() {
		app.Properties.Template = forRestore(base)
	}
	app.Properties.Configuration.Ingress.Traffic = withLabel(
		app.Properties.Configuration.Ingress.Traffic, names.Label, planned.Revision)
	if err := t.put(ctx, service.PreviewApp, *app); err != nil {
		return err
	}

	return t.verify(ctx, service.PreviewApp, planned.Revision, planned.Image, service.Config.Container, live)
}

// deactivate is the cleanup after a failed `up`: every revision minted this run
// is stopped. Errors are dropped — the failure being reported is the one that
// matters.
func (t *Target) deactivate(ctx context.Context, minted []ServiceResult) {
	for _, service := range minted {
		_, _ = t.Clients.Revisions.DeactivateRevision(
			ctx, t.Clients.ResourceGroup, service.App, service.Revision, nil)
	}
}

// retire deactivates the pull request's earlier active revisions on one app
// and untags their images, keeping only this push's.
func (t *Target) retire(
	ctx context.Context, app, repository string, names Names, keepRevision, keepTag string,
) error {
	revisions, err := t.activeRevisions(ctx, app)
	if err != nil {
		return err
	}
	for _, revision := range staleRevisions(revisions, revisionName(app, names.SuffixPrefix), keepRevision) {
		if _, err := t.Clients.Revisions.DeactivateRevision(
			ctx, t.Clients.ResourceGroup, app, revision, nil); err != nil {
			return err
		}
		t.printf("    deactivated %s\n", revision)
	}

	tags, err := t.Clients.TagsWithPrefix(ctx, t.Login, repository, names.TagPrefix)
	if err != nil {
		return err
	}
	for _, tag := range staleTags(tags, keepTag) {
		if err := t.Clients.DeleteTag(ctx, t.Login, repository, tag); err != nil {
			return err
		}
		t.printf("    untagged %s:%s\n", repository, tag)
	}
	return nil
}

// staleRevisions are the active revisions under a pull request's prefix other
// than the one just minted.
func staleRevisions(active []string, prefix, keep string) []string {
	var stale []string
	for _, revision := range active {
		if strings.HasPrefix(revision, prefix) && revision != keep {
			stale = append(stale, revision)
		}
	}
	return stale
}

// staleTags are a pull request's tags other than this push's. The tags were
// listed by prefix, so the only question is which one to keep.
func staleTags(tags []string, keep string) []string {
	var stale []string
	for _, tag := range tags {
		if tag != keep {
			stale = append(stale, tag)
		}
	}
	return stale
}

// verify proves the revision exists and serves this push's image, and — when the
// preview shares the service app — that the restore actually restored. Container
// Apps declines to mint a revision whose template matches an existing one, so a
// successful PUT is not evidence.
//
// `live` is empty for a dedicated app, where there is nothing to restore and so
// nothing to check.
func (t *Target) verify(ctx context.Context, app, revision, image, container, live string) error {
	response, err := t.Clients.Revisions.GetRevision(ctx, t.Clients.ResourceGroup, app, revision, nil)
	if err != nil {
		if azure.NotFound(err) {
			return fmt.Errorf("%s was not created — the template matched an existing revision", revision)
		}
		return err
	}
	if got := imageOf(response.Properties.Template, container); got != image {
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
	want := imageOf(liveResponse.Properties.Template, container)
	if got := imageOf(current.Properties.Template, container); got != want {
		return fmt.Errorf(
			"%s is left holding %q rather than %s's %q — restore it before releasing",
			app, got, live, want)
	}
	return nil
}

// ── Down ─────────────────────────────────────────────────────────────────

// DownResult is what `down` removed.
type DownResult struct {
	PR       int                 `json:"pr"`
	Label    string              `json:"label"`
	Database string              `json:"database"`
	Dropped  bool                `json:"databaseDropped"`
	Services []DownServiceResult `json:"services"`
}

// DownServiceResult is one service's share of a DownResult.
type DownServiceResult struct {
	Name         string   `json:"name"`
	App          string   `json:"app"`
	LabelRemoved bool     `json:"labelRemoved"`
	Deactivated  []string `json:"deactivated"`
	Untagged     []string `json:"untagged"`
}

// Destroy removes everything a pull request created.
func (t *Target) Destroy(ctx context.Context, pr int) (DownResult, error) {
	label, err := t.Label(ctx, pr)
	if err != nil {
		return DownResult{}, err
	}
	names := t.Names(pr, label)
	result := DownResult{PR: pr, Label: label, Database: names.Database}

	for i, service := range t.Services {
		serviceResult := DownServiceResult{
			Name: service.Name, App: service.PreviewApp, Deactivated: []string{}, Untagged: []string{},
		}

		t.printf("==> %s label\n", service.PreviewApp)
		app, err := t.app(ctx, service.PreviewApp)
		if err != nil {
			return result, err
		}
		switch {
		case app == nil:
			t.printf("    no %s\n", service.PreviewApp)
		case label == "":
			t.printf("    none held\n")
		case hasLabel(app.Properties.Configuration.Ingress.Traffic, label):
			app.Properties.Configuration.Ingress.Traffic = withoutLabel(
				app.Properties.Configuration.Ingress.Traffic, label)
			if err := t.put(ctx, service.PreviewApp, *app); err != nil {
				return result, err
			}
			serviceResult.LabelRemoved = true
			t.printf("    removed %s\n", label)
		default:
			t.printf("    no %s\n", label)
		}

		// Deactivating stops the replicas and the billing. It does NOT free the
		// revision slot: an app holds 100 revisions, active and inactive, and the
		// oldest are purged past that. maxInactiveRevisions on the app is what stops
		// pull request churn evicting rollback history.
		t.printf("==> %s revisions\n", service.PreviewApp)
		prefix := revisionName(service.PreviewApp, names.SuffixPrefix)
		if app != nil {
			active, err := t.activeRevisions(ctx, service.PreviewApp)
			if err != nil {
				return result, err
			}
			for _, revision := range staleRevisions(active, prefix, "") {
				if _, err := t.Clients.Revisions.DeactivateRevision(
					ctx, t.Clients.ResourceGroup, service.PreviewApp, revision, nil); err != nil {
					return result, err
				}
				serviceResult.Deactivated = append(serviceResult.Deactivated, revision)
				t.printf("    deactivated %s\n", revision)
			}
		}
		if len(serviceResult.Deactivated) == 0 {
			t.printf("    none matching %s*\n", prefix)
		}

		// Found by prefix: the pull request pushed one tag per commit.
		repository := names.Services[i].Repository
		t.printf("==> %s image tags\n", repository)
		tags, err := t.Clients.TagsWithPrefix(ctx, t.Login, repository, names.TagPrefix)
		if err != nil {
			return result, err
		}
		for _, tag := range tags {
			if err := t.Clients.DeleteTag(ctx, t.Login, repository, tag); err != nil {
				return result, err
			}
			serviceResult.Untagged = append(serviceResult.Untagged, tag)
			t.printf("    untagged %s:%s\n", repository, tag)
		}
		if len(tags) == 0 {
			t.printf("    none matching %s*\n", names.TagPrefix)
		}

		result.Services = append(result.Services, serviceResult)
	}

	t.printf("==> Database\n")
	if t.Server == "" {
		t.printf("    no database server in the deployment outputs\n")
	} else {
		dropped, err := t.Clients.DropDatabase(ctx, t.Server, names.Database)
		if err != nil {
			return result, err
		}
		result.Dropped = dropped
		if dropped {
			t.printf("    dropped %s\n", names.Database)
		} else {
			t.printf("    no %s\n", names.Database)
		}
	}
	return result, nil
}

// ── Status ───────────────────────────────────────────────────────────────

// StatusResult is what `status` found.
type StatusResult struct {
	PR       int                   `json:"pr"`
	Exists   bool                  `json:"exists"`
	Label    string                `json:"label"`
	Database DatabaseStatus        `json:"database"`
	Services []ServiceStatusResult `json:"services"`
}

// DatabaseStatus is the database's share of a StatusResult. Exists is nil
// when there is no server to ask.
type DatabaseStatus struct {
	Name   string `json:"name"`
	Exists *bool  `json:"exists"`
}

// ServiceStatusResult is one service's share of a StatusResult. Healthy is
// nil when there is no revision to ask about.
type ServiceStatusResult struct {
	Name     string `json:"name"`
	App      string `json:"app"`
	Label    string `json:"label"`
	Revision string `json:"revision"`
	URL      string `json:"url"`
	Healthy  *bool  `json:"healthy"`
}

// Status reports what exists for a pull request. Exists is false when no app
// carries its label.
func (t *Target) Status(ctx context.Context, pr int) (StatusResult, error) {
	label, err := t.Label(ctx, pr)
	if err != nil {
		return StatusResult{}, err
	}
	names := t.Names(pr, label)
	result := StatusResult{PR: pr, Label: label, Database: DatabaseStatus{Name: names.Database}}

	for i, service := range t.Services {
		serviceResult := ServiceStatusResult{Name: service.Name, App: service.PreviewApp, Label: label}
		app, err := t.app(ctx, service.PreviewApp)
		if err != nil {
			return result, err
		}
		if app != nil && label != "" && app.Properties != nil && app.Properties.Configuration != nil &&
			app.Properties.Configuration.Ingress != nil {
			for _, entry := range app.Properties.Configuration.Ingress.Traffic {
				if entry != nil && entry.Label != nil && *entry.Label == label && entry.RevisionName != nil {
					serviceResult.Revision = *entry.RevisionName
				}
			}
		}
		if serviceResult.Revision != "" {
			result.Exists = true
			serviceResult.URL = names.Services[i].URL
			response, err := t.Clients.Revisions.GetRevision(
				ctx, t.Clients.ResourceGroup, service.PreviewApp, serviceResult.Revision, nil)
			if err != nil && !azure.NotFound(err) {
				return result, err
			}
			healthy := err == nil && response.Properties != nil && response.Properties.HealthState != nil &&
				*response.Properties.HealthState == armappcontainers.RevisionHealthStateHealthy
			serviceResult.Healthy = &healthy
		}
		result.Services = append(result.Services, serviceResult)
	}

	if t.Server != "" && label != "" {
		exists, err := t.Clients.DatabaseExists(ctx, t.Server, names.Database)
		if err != nil {
			return result, err
		}
		result.Database.Exists = &exists
	}
	return result, nil
}

// ── Promote ──────────────────────────────────────────────────────────────

// PromoteResult is what `promote` moved traffic to.
type PromoteResult struct {
	PR       int                    `json:"pr"`
	Label    string                 `json:"label"`
	Services []PromoteServiceResult `json:"services"`
}

// PromoteServiceResult is one service's share of a PromoteResult.
type PromoteServiceResult struct {
	Name     string `json:"name"`
	App      string `json:"app"`
	Revision string `json:"revision"`
}

// Promote moves the pull request's labelled revision to 100% of traffic on a
// SHARED app — one where the preview app is the app azd deploys, so the
// revision is already a release of the right thing. A dedicated preview app is
// refused: promoting there changes nothing anybody uses, and the release path
// is azd's.
//
// The label stays attached. Plot's promoteRevision does the same, and it keeps
// the preview URL answering while the pull request is still open.
func (t *Target) Promote(ctx context.Context, pr int, only string) (PromoteResult, error) {
	label, err := t.Label(ctx, pr)
	if err != nil {
		return PromoteResult{}, err
	}
	if label == "" {
		return PromoteResult{}, fmt.Errorf("pull request %d holds no preview label", pr)
	}
	result := PromoteResult{PR: pr, Label: label}

	for _, service := range t.Services {
		if only != "" && service.Name != only {
			continue
		}
		if service.dedicated() {
			return result, fmt.Errorf(
				"%s is a dedicated preview app, not what %s serves — deploy through azd instead",
				service.PreviewApp, service.ServiceApp)
		}
		app, err := t.app(ctx, service.PreviewApp)
		if err != nil {
			return result, err
		}
		if app == nil {
			return result, fmt.Errorf("no app %s", service.PreviewApp)
		}
		traffic, revision, err := promoted(app.Properties.Configuration.Ingress.Traffic, label)
		if err != nil {
			return result, fmt.Errorf("%s: %w", service.PreviewApp, err)
		}
		t.printf("==> Promoting %s on %s\n", revision, service.PreviewApp)
		app.Properties.Configuration.Ingress.Traffic = traffic
		if err := t.put(ctx, service.PreviewApp, *app); err != nil {
			return result, err
		}
		result.Services = append(result.Services, PromoteServiceResult{
			Name: service.Name, App: service.PreviewApp, Revision: revision,
		})
	}
	if len(result.Services) == 0 {
		return result, fmt.Errorf("no service named %q", only)
	}
	return result, nil
}

// promoted is the traffic array with the labelled entry at 100 and everything
// else at zero. Entries are kept, labels and all — a weight of zero is how a
// label keeps its URL.
func promoted(
	traffic []*armappcontainers.TrafficWeight, label string,
) ([]*armappcontainers.TrafficWeight, string, error) {
	revision := ""
	updated := make([]*armappcontainers.TrafficWeight, 0, len(traffic))
	for _, entry := range traffic {
		if entry == nil {
			continue
		}
		copied := *entry
		if copied.Label != nil && *copied.Label == label {
			if copied.RevisionName == nil {
				return nil, "", fmt.Errorf("label %s points at no revision", label)
			}
			revision = *copied.RevisionName
			copied.Weight = to.Ptr(int32(100))
		} else {
			copied.Weight = to.Ptr(int32(0))
		}
		updated = append(updated, &copied)
	}
	if revision == "" {
		return nil, "", fmt.Errorf("no revision carries the label %s", label)
	}
	return updated, revision, nil
}

// ── Shared ───────────────────────────────────────────────────────────────

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

// baseRevision is the revision a shared app's preview is derived from: the one
// carrying the live label, else the one holding 100% of traffic — by name, or
// through `latestRevision: true`, which is how a fresh `azd deploy` leaves the
// split.
//
// This used to fall back to the app's latestRevisionName, which on an app that
// hosts previews is a PREVIEW: the last one minted, or the restore revision
// behind it. Deriving from that gave the next pull request the last one's
// database. A weight of 100 is what is actually being served, and a preview
// never holds any.
func baseRevision(app *armappcontainers.ContainerApp, liveLabel string) (string, error) {
	traffic := app.Properties.Configuration.Ingress.Traffic
	for _, entry := range traffic {
		if entry != nil && entry.Label != nil && *entry.Label == liveLabel && entry.RevisionName != nil {
			return *entry.RevisionName, nil
		}
	}
	for _, entry := range traffic {
		if entry == nil || entry.Weight == nil || *entry.Weight != 100 {
			continue
		}
		if entry.RevisionName != nil && *entry.RevisionName != "" {
			return *entry.RevisionName, nil
		}
		if entry.LatestRevision != nil && *entry.LatestRevision &&
			app.Properties.LatestRevisionName != nil && *app.Properties.LatestRevisionName != "" {
			return *app.Properties.LatestRevisionName, nil
		}
	}
	return "", fmt.Errorf(
		"%s has no revision labelled %s and none holding 100%% of traffic — label the live one",
		*app.Name, liveLabel)
}

// liveRevision is the revision a shared app's preview inherits from, and its
// template.
func (t *Target) liveRevision(
	ctx context.Context, app *armappcontainers.ContainerApp,
) (string, *armappcontainers.Template, error) {
	name, err := baseRevision(app, t.Config.LiveLabel)
	if err != nil {
		return "", nil, err
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

// activeRevisions lists an app's active revisions by name. Deactivating an
// inactive revision is an error, not a no-op, so the inactive ones are left
// out here.
func (t *Target) activeRevisions(ctx context.Context, app string) ([]string, error) {
	var found []string
	pager := t.Clients.Revisions.NewListRevisionsPager(t.Clients.ResourceGroup, app, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, revision := range page.Value {
			if revision == nil || revision.Name == nil {
				continue
			}
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

// provision runs the repository's provision command with every service's
// overrides in its environment, in declaration order — a later service's
// value wins where two name the same variable. With one service that is
// exactly its `env`.
func (t *Target) provision(ctx context.Context, names Names) error {
	env := map[string]string{}
	for i, service := range t.Services {
		expanded, err := config.ExpandFull(service.Config.Env, t.vars(names, names.Services[i].URL))
		if err != nil {
			return err
		}
		for key, value := range expanded {
			env[key] = value
		}
	}
	return t.run(ctx, names, env)
}

// run executes the repository's provision command with the environment a
// migration needs.
func (t *Target) run(ctx context.Context, names Names, env map[string]string) error {
	// Every deployment output, then provisionEnv, then the stage-specific
	// values, each layer overriding the one before. The outputs go in wholesale
	// because azd hands an extension its environment over gRPC, so a subprocess
	// inherits none of them.
	resolved := map[string]string{}
	for key, value := range t.Outputs {
		resolved[key] = value
	}

	url := ""
	if len(names.Services) > 0 {
		url = names.Services[0].URL
	}
	fromConfig, err := config.ExpandFull(t.Config.ProvisionEnv, t.vars(names, url))
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
	command.Stdout = io.MultiWriter(t.log(), &captured)
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

// containerIndex finds the container the preview replaces: the named one, or
// the first when no name is given.
func containerIndex(template *armappcontainers.Template, name string) (int, error) {
	if template == nil || len(template.Containers) == 0 {
		return -1, fmt.Errorf("the template has no containers")
	}
	if name == "" {
		return 0, nil
	}
	for i, container := range template.Containers {
		if container != nil && container.Name != nil && *container.Name == name {
			return i, nil
		}
	}
	return -1, fmt.Errorf("the template has no container named %q", name)
}

// imageOf is the image the named (or first) container runs, or "".
func imageOf(template *armappcontainers.Template, container string) string {
	index, err := containerIndex(template, container)
	if err != nil || template.Containers[index] == nil || template.Containers[index].Image == nil {
		return ""
	}
	return *template.Containers[index].Image
}

func serving(ctx context.Context, url string) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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

func scaleOf(scale *config.Scale) (int32, int32) {
	// Scales to zero, so an idle preview costs nothing.
	scaleMin, scaleMax := int32(0), int32(1)
	if scale != nil {
		if scale.Min != nil {
			scaleMin = *scale.Min
		}
		if scale.Max != nil {
			scaleMax = *scale.Max
		}
	}
	return scaleMin, scaleMax
}

// previewTemplate derives a preview revision from the template production is
// serving, rather than restating probes, resources and environment in a config
// file that would drift. A revision inherits the app's identity, registries and
// secret references natively, so unlike an app per pull request there is nothing
// to copy but the template itself.
//
// Everything in the template comes along — init containers, volumes, service
// binds, every container — because an app with a sidecar or a mounted share is
// not the same app without them. Only the named container's image and env
// change; the scale is replaced, since a preview does not want production's
// floor or its rules.
func previewTemplate(
	base *armappcontainers.Template,
	image string,
	container string,
	overrides map[string]string,
	suffix string,
	scaleMin, scaleMax int32,
) (*armappcontainers.Template, error) {
	index, err := containerIndex(base, container)
	if err != nil {
		return nil, err
	}

	containers := make([]*armappcontainers.Container, len(base.Containers))
	for i, source := range base.Containers {
		if source == nil {
			continue
		}
		copied := *source
		containers[i] = &copied
	}

	target := containers[index]
	target.Image = to.Ptr(image)
	target.Env = withOverrides(target.Env, overrides)

	template := *base
	template.RevisionSuffix = to.Ptr(suffix)
	template.Containers = containers
	template.Scale = &armappcontainers.Scale{
		MinReplicas: to.Ptr(scaleMin),
		MaxReplicas: to.Ptr(scaleMax),
	}
	return &template, nil
}

// withOverrides is a container's env with values replaced or appended. The
// source is not touched: the caller still needs the live template intact for
// the restore.
func withOverrides(
	source []*armappcontainers.EnvironmentVar, overrides map[string]string,
) []*armappcontainers.EnvironmentVar {
	pending := make(map[string]string, len(overrides))
	for key, value := range overrides {
		pending[key] = value
	}

	env := make([]*armappcontainers.EnvironmentVar, 0, len(source)+len(pending))
	for _, entry := range source {
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
	// Sorted, so two runs with the same overrides write the same template —
	// Container Apps compares templates to decide whether to mint.
	for _, key := range sortedKeys(pending) {
		env = append(env, &armappcontainers.EnvironmentVar{Name: to.Ptr(key), Value: to.Ptr(pending[key])})
	}
	return env
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
