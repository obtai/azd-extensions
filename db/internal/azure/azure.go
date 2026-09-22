// Package azure wraps the Azure SDK clients this extension needs. Everything
// goes through the SDK rather than the `az` CLI, so "does not exist" is a 404
// on a typed error rather than an unparseable failure that looks like an answer.
package azure

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/postgresql/armpostgresqlflexibleservers/v4"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions"
)

// NewCredential is explicit rather than DefaultAzureCredential, whose silent
// fallbacks turn a misconfiguration into a confusing failure several steps later.
// Copied from the shell extension: each extension is its own module.
//
// tenant comes from the azd environment where it has one. The Azure CLI's own
// default can be the literal "common", which the token endpoint rejects.
func NewCredential(tenant string) (azcore.TokenCredential, error) {
	// Container Apps injects IDENTITY_ENDPOINT.
	if endpoint := firstSet("IDENTITY_ENDPOINT", "MSI_ENDPOINT"); endpoint != "" {
		clientID := firstSet("AZURE_CLIENT_ID")
		if clientID == "" {
			return nil, errors.New(
				"AZURE_CLIENT_ID is required to select the user-assigned managed identity")
		}
		return azidentity.NewManagedIdentityCredential(&azidentity.ManagedIdentityCredentialOptions{
			ID: azidentity.ClientID(clientID),
		})
	}
	if tenant == "common" {
		tenant = ""
	}
	return azidentity.NewAzureCLICredential(&azidentity.AzureCLICredentialOptions{TenantID: tenant})
}

func firstSet(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

// Subscriptions lists the subscriptions the credential can see.
func Subscriptions(ctx context.Context, credential azcore.TokenCredential) ([]*armsubscriptions.Subscription, error) {
	client, err := armsubscriptions.NewClient(credential, nil)
	if err != nil {
		return nil, err
	}
	var subscriptions []*armsubscriptions.Subscription
	pager := client.NewListPager(nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing subscriptions: %w", err)
		}
		subscriptions = append(subscriptions, page.Value...)
	}
	return subscriptions, nil
}

// Clients is the flexible server control plane for one subscription.
type Clients struct {
	SubscriptionID string

	servers   *armpostgresqlflexibleservers.ServersClient
	admins    *armpostgresqlflexibleservers.AdministratorsClient
	databases *armpostgresqlflexibleservers.DatabasesClient
	firewall  *armpostgresqlflexibleservers.FirewallRulesClient
}

// New builds the clients for a subscription.
func New(subscriptionID string, credential azcore.TokenCredential) (*Clients, error) {
	factory, err := armpostgresqlflexibleservers.NewClientFactory(subscriptionID, credential, nil)
	if err != nil {
		return nil, err
	}
	return &Clients{
		SubscriptionID: subscriptionID,
		servers:        factory.NewServersClient(),
		admins:         factory.NewAdministratorsClient(),
		databases:      factory.NewDatabasesClient(),
		firewall:       factory.NewFirewallRulesClient(),
	}, nil
}

// Servers lists the flexible servers in a resource group, or in the whole
// subscription when resourceGroup is empty.
func (c *Clients) Servers(ctx context.Context, resourceGroup string) ([]*armpostgresqlflexibleservers.Server, error) {
	var servers []*armpostgresqlflexibleservers.Server
	if resourceGroup == "" {
		pager := c.servers.NewListPager(nil)
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("listing flexible servers: %w", err)
			}
			servers = append(servers, page.Value...)
		}
		return servers, nil
	}

	pager := c.servers.NewListByResourceGroupPager(resourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing flexible servers in %s: %w", resourceGroup, err)
		}
		servers = append(servers, page.Value...)
	}
	return servers, nil
}

// Databases lists a server's databases, system ones included — the caller
// decides what to hide.
func (c *Clients) Databases(ctx context.Context, resourceGroup, server string) ([]*armpostgresqlflexibleservers.Database, error) {
	var databases []*armpostgresqlflexibleservers.Database
	pager := c.databases.NewListByServerPager(resourceGroup, server, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing databases on %s: %w", server, err)
		}
		databases = append(databases, page.Value...)
	}
	return databases, nil
}

// Administrators lists the server's Entra administrators, whose principal
// names are Postgres role names.
func (c *Clients) Administrators(ctx context.Context, resourceGroup, server string) ([]string, error) {
	var names []string
	pager := c.admins.NewListByServerPager(resourceGroup, server, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing Entra administrators of %s: %w", server, err)
		}
		for _, admin := range page.Value {
			if admin.Properties != nil && admin.Properties.PrincipalName != nil {
				names = append(names, *admin.Properties.PrincipalName)
			}
		}
	}
	return names, nil
}

// FirewallRules lists a server's firewall rules.
func (c *Clients) FirewallRules(ctx context.Context, resourceGroup, server string) ([]*armpostgresqlflexibleservers.FirewallRule, error) {
	var rules []*armpostgresqlflexibleservers.FirewallRule
	pager := c.firewall.NewListByServerPager(resourceGroup, server, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing firewall rules on %s: %w", server, err)
		}
		rules = append(rules, page.Value...)
	}
	return rules, nil
}

// AddFirewallRule opens one address. The only write this extension makes, and
// only ever after someone has said yes to it.
func (c *Clients) AddFirewallRule(ctx context.Context, resourceGroup, server, name, address string) error {
	poller, err := c.firewall.BeginCreateOrUpdate(ctx, resourceGroup, server, name,
		armpostgresqlflexibleservers.FirewallRule{
			Properties: &armpostgresqlflexibleservers.FirewallRuleProperties{
				StartIPAddress: to.Ptr(address),
				EndIPAddress:   to.Ptr(address),
			},
		}, nil)
	if err != nil {
		return fmt.Errorf("adding firewall rule %s to %s: %w", name, server, err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("adding firewall rule %s to %s: %w", name, server, err)
	}
	return nil
}

// ResourceGroupOf reads the resource group out of an ARM resource ID.
func ResourceGroupOf(id string) string {
	parsed, err := arm.ParseResourceID(id)
	if err != nil {
		return ""
	}
	return parsed.ResourceGroupName
}
