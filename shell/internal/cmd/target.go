package cmd

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
	"github.com/azure/azure-dev/cli/azd/pkg/azdext"

	"github.com/obtai/azd-extensions/shell/internal/azure"
	"github.com/obtai/azd-extensions/shell/internal/ui"
)

// serviceTag is how azd marks the app it deploys a service to.
const serviceTag = "azd-service-name"

type options struct {
	subscription  string
	resourceGroup string
	app           string
	revision      string
	replica       string
	container     string
	command       string
	quiet         bool
}

// target is one container, resolved.
type target struct {
	clients       *azure.Clients
	resourceGroup string
	app           string
	revision      string
	replica       string
	container     *armappcontainers.ReplicaContainer
}

// resolve walks subscription → resource group → app → revision → replica →
// container, taking each from a flag, the azd environment, the only choice,
// or a picker, in that order.
func resolve(ctx context.Context, u ui.UI, environment string, opts options) (*target, error) {
	values, err := azdValues(ctx, environment)
	if err != nil {
		return nil, err
	}
	lookup := func(key string) string {
		return cmp.Or(values[key], os.Getenv(key))
	}

	credential, err := azure.NewCredential()
	if err != nil {
		return nil, err
	}

	subscription := cmp.Or(opts.subscription, lookup("AZURE_SUBSCRIPTION_ID"))
	if subscription == "" {
		subscriptions, err := ui.Loading(ctx, u, "Finding subscriptions…", func(ctx context.Context) ([]ui.Item[string], error) {
			found, err := azure.Subscriptions(ctx, credential)
			if err != nil {
				return nil, err
			}
			items := make([]ui.Item[string], 0, len(found))
			for _, s := range found {
				items = append(items, ui.Item[string]{
					Label: fmt.Sprintf("%s (%s)", deref(s.DisplayName), deref(s.SubscriptionID)),
					Value: deref(s.SubscriptionID),
				})
			}
			slices.SortFunc(items, func(a, b ui.Item[string]) int { return strings.Compare(a.Label, b.Label) })
			return items, nil
		})
		if err != nil {
			return nil, err
		}
		subscription, err = ui.Pick(ctx, u, ui.Choice{Title: "Subscription", Noun: "subscriptions", Flag: "--subscription"}, subscriptions)
		if err != nil {
			return nil, err
		}
	}

	clients, err := azure.New(subscription, credential)
	if err != nil {
		return nil, err
	}
	t := &target{clients: clients}

	app, err := resolveApp(ctx, u, clients, cmp.Or(opts.resourceGroup, lookup("AZURE_RESOURCE_GROUP")), opts.app)
	if err != nil {
		return nil, err
	}
	t.app = deref(app.Name)
	t.resourceGroup = azure.ResourceGroupOf(deref(app.ID))

	t.revision = opts.revision
	if t.revision == "" {
		revisions, err := ui.Loading(ctx, u, "Finding revisions…", func(ctx context.Context) ([]*armappcontainers.Revision, error) {
			return clients.ActiveRevisions(ctx, t.resourceGroup, t.app)
		})
		if err != nil {
			return nil, err
		}
		if len(revisions) == 0 {
			return nil, fmt.Errorf("%s has no active revisions", t.app)
		}
		sortRevisions(revisions)
		items := make([]ui.Item[string], len(revisions))
		for i, revision := range revisions {
			items[i] = ui.Item[string]{Label: revisionLabel(revision), Value: deref(revision.Name)}
		}
		t.revision, err = ui.Pick(ctx, u, ui.Choice{Title: "Revision", Noun: "active revisions", Flag: "--revision"}, items)
		if err != nil {
			return nil, err
		}
	}

	replicas, err := ui.Loading(ctx, u, "Finding replicas…", func(ctx context.Context) ([]*armappcontainers.Replica, error) {
		return clients.Replicas(ctx, t.resourceGroup, t.app, t.revision)
	})
	if err != nil {
		return nil, err
	}
	if len(replicas) == 0 {
		return nil, fmt.Errorf("revision %s has no running replicas (scaled to zero?). "+
			"Hit the app's URL to wake it, or set minReplicas", t.revision)
	}
	var replica *armappcontainers.Replica
	if opts.replica != "" {
		for _, r := range replicas {
			if deref(r.Name) == opts.replica {
				replica = r
			}
		}
		if replica == nil {
			return nil, fmt.Errorf("revision %s has no replica %s", t.revision, opts.replica)
		}
	} else {
		items := make([]ui.Item[*armappcontainers.Replica], len(replicas))
		for i, r := range replicas {
			items[i] = ui.Item[*armappcontainers.Replica]{Label: replicaLabel(r), Value: r}
		}
		replica, err = ui.Pick(ctx, u, ui.Choice{Title: "Replica", Noun: "replicas", Flag: "--replica"}, items)
		if err != nil {
			return nil, err
		}
	}
	t.replica = deref(replica.Name)

	var containers []*armappcontainers.ReplicaContainer
	if replica.Properties != nil {
		containers = replica.Properties.Containers
	}
	if opts.container != "" {
		for _, c := range containers {
			if deref(c.Name) == opts.container {
				t.container = c
			}
		}
		if t.container == nil {
			return nil, fmt.Errorf("replica %s has no container %s", t.replica, opts.container)
		}
		return t, nil
	}
	items := make([]ui.Item[*armappcontainers.ReplicaContainer], len(containers))
	for i, c := range containers {
		items[i] = ui.Item[*armappcontainers.ReplicaContainer]{Label: containerLabel(c), Value: c}
	}
	t.container, err = ui.Pick(ctx, u, ui.Choice{Title: "Container", Noun: "containers", Flag: "--container"}, items)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// resolveApp lists the apps in scope — the resource group, or the whole
// subscription — narrows them by name, and picks one. Across a subscription
// the resource group is picked first, as ecsgo picks a cluster.
func resolveApp(ctx context.Context, u ui.UI, clients *azure.Clients, resourceGroup, name string) (*armappcontainers.ContainerApp, error) {
	apps, err := ui.Loading(ctx, u, "Finding container apps…", func(ctx context.Context) ([]*armappcontainers.ContainerApp, error) {
		return clients.Apps(ctx, resourceGroup)
	})
	if err != nil {
		return nil, err
	}

	scope := "subscription " + clients.SubscriptionID
	if resourceGroup != "" {
		scope = "resource group " + resourceGroup
	}
	if len(apps) == 0 {
		return nil, fmt.Errorf("no container apps in %s", scope)
	}
	if name != "" {
		apps = matchApps(apps, name)
		if len(apps) == 0 {
			return nil, fmt.Errorf("no container app or azd service named %s in %s", name, scope)
		}
	}

	acrossGroups := resourceGroup == "" && len(groupsOf(apps)) > 1
	if acrossGroups && name == "" {
		groups := groupsOf(apps)
		items := make([]ui.Item[string], len(groups))
		for i, group := range groups {
			items[i] = ui.Item[string]{Label: group, Value: group}
		}
		group, err := ui.Pick(ctx, u, ui.Choice{Title: "Resource group", Noun: "resource groups", Flag: "--resource-group"}, items)
		if err != nil {
			return nil, err
		}
		apps = slices.DeleteFunc(apps, func(app *armappcontainers.ContainerApp) bool {
			return !strings.EqualFold(azure.ResourceGroupOf(deref(app.ID)), group)
		})
		acrossGroups = false
	}

	slices.SortFunc(apps, func(a, b *armappcontainers.ContainerApp) int {
		return strings.Compare(appLabel(a, acrossGroups), appLabel(b, acrossGroups))
	})
	items := make([]ui.Item[*armappcontainers.ContainerApp], len(apps))
	for i, app := range apps {
		items[i] = ui.Item[*armappcontainers.ContainerApp]{Label: appLabel(app, acrossGroups), Value: app}
	}
	choice := ui.Choice{Title: "Container app", Noun: "container apps", Flag: "--app"}
	if name != "" {
		// Named, and still ambiguous: the same service in several groups.
		choice.Flag = "--resource-group"
	}
	return ui.Pick(ctx, u, choice, items)
}

// matchApps finds apps by their own name or by the azd service they host.
func matchApps(apps []*armappcontainers.ContainerApp, name string) []*armappcontainers.ContainerApp {
	var matched []*armappcontainers.ContainerApp
	for _, app := range apps {
		if strings.EqualFold(deref(app.Name), name) || strings.EqualFold(service(app), name) {
			matched = append(matched, app)
		}
	}
	return matched
}

func service(app *armappcontainers.ContainerApp) string {
	return deref(app.Tags[serviceTag])
}

// groupsOf is the sorted, distinct resource groups the apps live in.
func groupsOf(apps []*armappcontainers.ContainerApp) []string {
	var groups []string
	for _, app := range apps {
		group := azure.ResourceGroupOf(deref(app.ID))
		if !slices.ContainsFunc(groups, func(g string) bool { return strings.EqualFold(g, group) }) {
			groups = append(groups, group)
		}
	}
	slices.Sort(groups)
	return groups
}

// appLabel leads with the azd service, which is the name people use.
func appLabel(app *armappcontainers.ContainerApp, withGroup bool) string {
	label := deref(app.Name)
	if s := service(app); s != "" {
		label = fmt.Sprintf("%s (%s)", s, label)
	}
	if withGroup {
		label += " · " + azure.ResourceGroupOf(deref(app.ID))
	}
	return label
}

// sortRevisions puts the revision taking traffic first, then the newest.
func sortRevisions(revisions []*armappcontainers.Revision) {
	slices.SortStableFunc(revisions, func(a, b *armappcontainers.Revision) int {
		if c := cmp.Compare(traffic(b), traffic(a)); c != 0 {
			return c
		}
		return created(b).Compare(created(a))
	})
}

func traffic(revision *armappcontainers.Revision) int32 {
	if revision.Properties == nil || revision.Properties.TrafficWeight == nil {
		return 0
	}
	return *revision.Properties.TrafficWeight
}

func created(revision *armappcontainers.Revision) time.Time {
	if revision.Properties == nil || revision.Properties.CreatedTime == nil {
		return time.Time{}
	}
	return *revision.Properties.CreatedTime
}

func revisionLabel(revision *armappcontainers.Revision) string {
	parts := []string{deref(revision.Name)}
	if weight := traffic(revision); weight > 0 {
		parts = append(parts, fmt.Sprintf("%d%% traffic", weight))
	}
	if revision.Properties != nil && revision.Properties.Replicas != nil {
		parts = append(parts, plural(int(*revision.Properties.Replicas), "replica"))
	}
	if t := created(revision); !t.IsZero() {
		parts = append(parts, "created "+ago(t))
	}
	return strings.Join(parts, " · ")
}

func replicaLabel(replica *armappcontainers.Replica) string {
	parts := []string{deref(replica.Name)}
	if p := replica.Properties; p != nil {
		if p.RunningState != nil {
			parts = append(parts, string(*p.RunningState))
		}
		if p.CreatedTime != nil {
			parts = append(parts, "started "+ago(*p.CreatedTime))
		}
	}
	return strings.Join(parts, " · ")
}

func containerLabel(container *armappcontainers.ReplicaContainer) string {
	parts := []string{deref(container.Name)}
	if container.RunningState != nil {
		parts = append(parts, string(*container.RunningState))
	}
	if container.RestartCount != nil && *container.RestartCount > 0 {
		parts = append(parts, plural(int(*container.RestartCount), "restart"))
	}
	return strings.Join(parts, " · ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// azdValues is the azd environment's values, or nothing: outside an azd
// project, or with no environment selected, every value falls through to a
// flag, the process environment, or a picker. An environment named with -e
// that cannot be read is an error, though — falling back would quietly connect
// somewhere other than where it was asked to.
func azdValues(ctx context.Context, environment string) (map[string]string, error) {
	client, err := azdext.NewAzdClient()
	if err != nil {
		return nil, explicit(environment, err)
	}
	defer client.Close()

	name := environment
	if name == "" {
		current, err := client.Environment().GetCurrent(ctx, &azdext.EmptyRequest{})
		if err != nil || current.Environment == nil {
			return nil, nil
		}
		name = current.Environment.Name
	}

	response, err := client.Environment().GetValues(ctx, &azdext.GetEnvironmentRequest{Name: name})
	if err != nil {
		return nil, explicit(environment, err)
	}
	values := make(map[string]string, len(response.KeyValues))
	for _, kv := range response.KeyValues {
		values[kv.Key] = kv.Value
	}
	return values, nil
}

func explicit(environment string, err error) error {
	if environment == "" {
		return nil
	}
	return fmt.Errorf("reading azd environment %s: %w", environment, err)
}
