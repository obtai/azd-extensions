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
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions"
)

// NewCredential is explicit rather than DefaultAzureCredential, whose silent
// fallbacks turn a misconfiguration into a confusing failure several steps later.
// Copied from the preview extension: each extension is its own module.
func NewCredential() (azcore.TokenCredential, error) {
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
	return azidentity.NewAzureCLICredential(nil)
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

// Clients is the Container Apps control plane for one subscription.
type Clients struct {
	SubscriptionID string

	apps      *armappcontainers.ContainerAppsClient
	revisions *armappcontainers.ContainerAppsRevisionsClient
	replicas  *armappcontainers.ContainerAppsRevisionReplicasClient
}

// New builds the clients for a subscription.
func New(subscriptionID string, credential azcore.TokenCredential) (*Clients, error) {
	factory, err := armappcontainers.NewClientFactory(subscriptionID, credential, nil)
	if err != nil {
		return nil, err
	}
	return &Clients{
		SubscriptionID: subscriptionID,
		apps:           factory.NewContainerAppsClient(),
		revisions:      factory.NewContainerAppsRevisionsClient(),
		replicas:       factory.NewContainerAppsRevisionReplicasClient(),
	}, nil
}

// Apps lists the container apps in a resource group, or in the whole
// subscription when resourceGroup is empty.
func (c *Clients) Apps(ctx context.Context, resourceGroup string) ([]*armappcontainers.ContainerApp, error) {
	var apps []*armappcontainers.ContainerApp
	if resourceGroup == "" {
		pager := c.apps.NewListBySubscriptionPager(nil)
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("listing container apps: %w", err)
			}
			apps = append(apps, page.Value...)
		}
		return apps, nil
	}

	pager := c.apps.NewListByResourceGroupPager(resourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing container apps in %s: %w", resourceGroup, err)
		}
		apps = append(apps, page.Value...)
	}
	return apps, nil
}

// ActiveRevisions lists an app's active revisions. Inactive ones have no
// replicas to connect to.
func (c *Clients) ActiveRevisions(ctx context.Context, resourceGroup, app string) ([]*armappcontainers.Revision, error) {
	var revisions []*armappcontainers.Revision
	pager := c.revisions.NewListRevisionsPager(resourceGroup, app, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing revisions of %s: %w", app, err)
		}
		for _, revision := range page.Value {
			if revision.Properties != nil && revision.Properties.Active != nil && *revision.Properties.Active {
				revisions = append(revisions, revision)
			}
		}
	}
	return revisions, nil
}

// Replicas lists a revision's running replicas.
func (c *Clients) Replicas(ctx context.Context, resourceGroup, app, revision string) ([]*armappcontainers.Replica, error) {
	response, err := c.replicas.ListReplicas(ctx, resourceGroup, app, revision, nil)
	if err != nil {
		return nil, fmt.Errorf("listing replicas of %s: %w", revision, err)
	}
	return response.Value, nil
}

// AuthToken is the short-lived token the exec endpoint accepts. It is scoped
// to the one app, which is why it is not the ARM token.
func (c *Clients) AuthToken(ctx context.Context, resourceGroup, app string) (string, error) {
	response, err := c.apps.GetAuthToken(ctx, resourceGroup, app, nil)
	if err != nil {
		return "", fmt.Errorf("getting an exec token for %s: %w", app, err)
	}
	if response.Properties == nil || response.Properties.Token == nil {
		return "", fmt.Errorf("getting an exec token for %s: the response carried no token", app)
	}
	return *response.Properties.Token, nil
}

// ResourceGroupOf reads the resource group out of an ARM resource ID.
func ResourceGroupOf(id string) string {
	parsed, err := arm.ParseResourceID(id)
	if err != nil {
		return ""
	}
	return parsed.ResourceGroupName
}
