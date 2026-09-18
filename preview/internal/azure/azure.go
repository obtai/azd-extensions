// Package azure wraps the Azure SDK clients this extension needs. Everything
// goes through the SDK rather than the `az` CLI, so "does not exist" is a 404
// on a typed error rather than an unparseable failure that looks like an answer.
package azure

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/postgresql/armpostgresqlflexibleservers/v4"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

// Clients is everything this extension talks to, for one subscription.
type Clients struct {
	SubscriptionID string
	ResourceGroup  string

	credential azcore.TokenCredential

	// Log is where progress goes. Stdout, unless a caller wants stdout for
	// something a script will parse.
	Log io.Writer

	ContainerApps *armappcontainers.ContainerAppsClient
	Revisions     *armappcontainers.ContainerAppsRevisionsClient
	Databases     *armpostgresqlflexibleservers.DatabasesClient
}

// New builds the clients for a subscription and resource group.
func New(ctx context.Context, subscriptionID, resourceGroup string) (*Clients, error) {
	credential, err := newCredential()
	if err != nil {
		return nil, err
	}

	apps, err := armappcontainers.NewClientFactory(subscriptionID, credential, nil)
	if err != nil {
		return nil, err
	}
	postgres, err := armpostgresqlflexibleservers.NewClientFactory(subscriptionID, credential, nil)
	if err != nil {
		return nil, err
	}

	return &Clients{
		SubscriptionID: subscriptionID,
		ResourceGroup:  resourceGroup,
		credential:     credential,
		Log:            os.Stdout,
		ContainerApps:  apps.NewContainerAppsClient(),
		Revisions:      apps.NewContainerAppsRevisionsClient(),
		Databases:      postgres.NewDatabasesClient(),
	}, nil
}

// newCredential is explicit rather than DefaultAzureCredential, whose silent
// fallbacks turn a misconfiguration into a confusing failure several steps later.
func newCredential() (azcore.TokenCredential, error) {
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

// Secrets is a data-plane client for one Key Vault.
func (c *Clients) Secrets(vaultName string) (*azsecrets.Client, error) {
	return azsecrets.NewClient("https://"+vaultName+".vault.azure.net", c.credential, nil)
}

// Credential exposes the shared credential for calls that build their own client.
func (c *Clients) Credential() azcore.TokenCredential { return c.credential }

// NotFound reports whether an error is Azure saying "no such thing". A 403 is
// not a 404 — see Forbidden.
func NotFound(err error) bool {
	var responseError *azcore.ResponseError
	return errors.As(err, &responseError) && responseError.StatusCode == http.StatusNotFound
}

// Forbidden reports whether Azure refused rather than denied existence.
func Forbidden(err error) bool {
	var responseError *azcore.ResponseError
	if !errors.As(err, &responseError) {
		return false
	}
	return responseError.StatusCode == http.StatusForbidden ||
		responseError.StatusCode == http.StatusUnauthorized
}

// Until polls check until it passes or the deadline expires.
func Until(ctx context.Context, timeout, interval time.Duration, check func(context.Context) bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if check(ctx) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}
}
