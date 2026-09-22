package cmd

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/postgresql/armpostgresqlflexibleservers/v4"
	"github.com/azure/azure-dev/cli/azd/pkg/azdext"

	"github.com/obtai/azd-extensions/db/internal/azure"
	"github.com/obtai/azd-extensions/db/internal/ui"
)

// systemDatabases are the ones nobody means. `postgres` is a real database,
// but connecting to it by accident instead of the app's is the mistake this
// extension exists to stop; --database names it when it is wanted.
var systemDatabases = []string{"azure_maintenance", "azure_sys", "postgres", "template0", "template1"}

// databaseKeys are how the blueprints spell the app's database in the azd
// environment. The prefix varies by repo — APP_, SALES_, none — so the suffix
// is matched instead of the whole name.
var databaseKeys = []string{"_DATABASE_NAME", "_POSTGRES_DATABASE", "PG_DATABASE", "_DATABASE"}

type options struct {
	subscription  string
	resourceGroup string
	server        string
	database      string
	user          string
	command       string
	url           bool
	quiet         bool
}

// target is one database, resolved.
type target struct {
	clients       *azure.Clients
	credential    azcore.TokenCredential
	resourceGroup string
	server        string
	host          string
	database      string
	tenant        string
}

// resolve walks subscription → resource group → server → database, taking each
// from a flag, the azd environment, the only choice, or a picker, in that order.
func resolve(ctx context.Context, u ui.UI, environment string, opts options) (*target, error) {
	values, err := azdValues(ctx, environment)
	if err != nil {
		return nil, err
	}
	lookup := func(key string) string {
		return cmp.Or(values[key], os.Getenv(key))
	}

	tenant := lookup("AZURE_TENANT_ID")
	credential, err := azure.NewCredential(tenant)
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
	t := &target{clients: clients, credential: credential, tenant: tenant}

	server, err := resolveServer(ctx, u, clients, cmp.Or(opts.resourceGroup, lookup("AZURE_RESOURCE_GROUP")), opts.server)
	if err != nil {
		return nil, err
	}
	if err := reachable(server); err != nil {
		return nil, err
	}
	t.server = deref(server.Name)
	t.resourceGroup = azure.ResourceGroupOf(deref(server.ID))
	t.host = fqdn(server)

	t.database = opts.database
	if t.database == "" {
		databases, err := ui.Loading(ctx, u, "Finding databases…", func(ctx context.Context) ([]*armpostgresqlflexibleservers.Database, error) {
			return clients.Databases(ctx, t.resourceGroup, t.server)
		})
		if err != nil {
			return nil, err
		}
		names := userDatabases(databases)
		if len(names) == 0 {
			return nil, fmt.Errorf("%s has no databases but the system ones; "+
				"pass --database to connect to one of those", t.server)
		}
		sortDatabases(names, namedDatabase(values))
		items := make([]ui.Item[string], len(names))
		for i, name := range names {
			items[i] = ui.Item[string]{Label: name, Value: name}
		}
		t.database, err = ui.Pick(ctx, u, ui.Choice{Title: "Database", Noun: "databases", Flag: "--database"}, items)
		if err != nil {
			return nil, err
		}
	}

	return t, nil
}

// resolveServer lists the flexible servers in scope — the resource group, or
// the whole subscription — narrows them by name, and picks one. Across a
// subscription the resource group is picked first, as `azd shell` does.
func resolveServer(ctx context.Context, u ui.UI, clients *azure.Clients, resourceGroup, name string) (*armpostgresqlflexibleservers.Server, error) {
	servers, err := ui.Loading(ctx, u, "Finding flexible servers…", func(ctx context.Context) ([]*armpostgresqlflexibleservers.Server, error) {
		return clients.Servers(ctx, resourceGroup)
	})
	if err != nil {
		return nil, err
	}

	scope := "subscription " + clients.SubscriptionID
	if resourceGroup != "" {
		scope = "resource group " + resourceGroup
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("no PostgreSQL flexible servers in %s", scope)
	}
	if name != "" {
		servers = slices.DeleteFunc(servers, func(server *armpostgresqlflexibleservers.Server) bool {
			return !strings.EqualFold(deref(server.Name), name)
		})
		if len(servers) == 0 {
			return nil, fmt.Errorf("no flexible server named %s in %s", name, scope)
		}
	}

	acrossGroups := resourceGroup == "" && len(groupsOf(servers)) > 1
	if acrossGroups && name == "" {
		groups := groupsOf(servers)
		items := make([]ui.Item[string], len(groups))
		for i, group := range groups {
			items[i] = ui.Item[string]{Label: group, Value: group}
		}
		group, err := ui.Pick(ctx, u, ui.Choice{Title: "Resource group", Noun: "resource groups", Flag: "--resource-group"}, items)
		if err != nil {
			return nil, err
		}
		servers = slices.DeleteFunc(servers, func(server *armpostgresqlflexibleservers.Server) bool {
			return !strings.EqualFold(azure.ResourceGroupOf(deref(server.ID)), group)
		})
		acrossGroups = false
	}

	slices.SortFunc(servers, func(a, b *armpostgresqlflexibleservers.Server) int {
		return strings.Compare(serverLabel(a, acrossGroups), serverLabel(b, acrossGroups))
	})
	items := make([]ui.Item[*armpostgresqlflexibleservers.Server], len(servers))
	for i, server := range servers {
		items[i] = ui.Item[*armpostgresqlflexibleservers.Server]{Label: serverLabel(server, acrossGroups), Value: server}
	}
	choice := ui.Choice{Title: "Flexible server", Noun: "flexible servers", Flag: "--server"}
	if name != "" {
		// Named, and still ambiguous: the same name in several groups.
		choice.Flag = "--resource-group"
	}
	return ui.Pick(ctx, u, choice, items)
}

// reachable refuses the two shapes this extension cannot do anything useful
// with, rather than handing psql a connection that will hang.
func reachable(server *armpostgresqlflexibleservers.Server) error {
	name := deref(server.Name)
	properties := server.Properties
	if properties == nil {
		return fmt.Errorf("%s reported no properties", name)
	}

	if network := properties.Network; network != nil {
		private := deref(network.DelegatedSubnetResourceID) != ""
		if network.PublicNetworkAccess != nil &&
			*network.PublicNetworkAccess == armpostgresqlflexibleservers.ServerPublicNetworkAccessStateDisabled {
			private = true
		}
		if private {
			return fmt.Errorf("%s has no public endpoint — it is in a virtual network, "+
				"and Container Apps has no port forwarding to tunnel through. "+
				"Run `azd shell` into an app on that network and use the client in its image", name)
		}
	}

	if auth := properties.AuthConfig; auth == nil || auth.ActiveDirectoryAuth == nil ||
		*auth.ActiveDirectoryAuth != armpostgresqlflexibleservers.ActiveDirectoryAuthEnumEnabled {
		return fmt.Errorf("%s has Microsoft Entra authentication disabled, and this extension "+
			"connects with an Entra token. It is a password-auth server: the login and password "+
			"are the Key Vault secrets DB-LOGIN and DB-PASSWORD", name)
	}
	return nil
}

func fqdn(server *armpostgresqlflexibleservers.Server) string {
	if server.Properties == nil {
		return ""
	}
	return deref(server.Properties.FullyQualifiedDomainName)
}

// userDatabases is everything but the ones Azure and Postgres put there.
func userDatabases(databases []*armpostgresqlflexibleservers.Database) []string {
	var names []string
	for _, database := range databases {
		name := deref(database.Name)
		if name == "" || slices.Contains(systemDatabases, name) {
			continue
		}
		names = append(names, name)
	}
	return names
}

// namedDatabase is the database the azd environment names, whatever prefix
// this repository gives it.
func namedDatabase(values map[string]string) string {
	var keys []string
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, suffix := range databaseKeys {
		for _, key := range keys {
			// PREVIEW_DATABASE_NAME is a database too, but not the one an
			// azd environment is about.
			if strings.HasSuffix(key, suffix) && !strings.Contains(key, "PREVIEW") && values[key] != "" {
				return values[key]
			}
		}
	}
	return ""
}

// sortDatabases puts the one the environment names first, then the rest
// alphabetically: `grid`, then `grid_preview`, then `grid_pr_95`.
func sortDatabases(names []string, first string) {
	slices.SortFunc(names, func(a, b string) int {
		if a == first {
			return -1
		}
		if b == first {
			return 1
		}
		return strings.Compare(a, b)
	})
}

// groupsOf is the sorted, distinct resource groups the servers live in.
func groupsOf(servers []*armpostgresqlflexibleservers.Server) []string {
	var groups []string
	for _, server := range servers {
		group := azure.ResourceGroupOf(deref(server.ID))
		if !slices.ContainsFunc(groups, func(g string) bool { return strings.EqualFold(g, group) }) {
			groups = append(groups, group)
		}
	}
	slices.Sort(groups)
	return groups
}

func serverLabel(server *armpostgresqlflexibleservers.Server, withGroup bool) string {
	parts := []string{deref(server.Name)}
	if properties := server.Properties; properties != nil && properties.Version != nil {
		parts = append(parts, "PostgreSQL "+string(*properties.Version))
	}
	if withGroup {
		parts = append(parts, azure.ResourceGroupOf(deref(server.ID)))
	}
	return strings.Join(parts, " · ")
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
