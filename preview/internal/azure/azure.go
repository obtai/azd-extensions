// Package azure wraps the Azure SDK clients this extension needs.
//
// Everything goes through the SDK rather than the `az` CLI. That is not a style
// preference — shelling out and reading stdout back produced the same class of
// failure repeatedly, and always the same shape: a failure that looked like an
// answer. A mistyped flag inside a silenced existence check is
// indistinguishable from "the resource does not exist", so a teardown once
// skipped a database it should have dropped and a closed pull request kept its
// data.
//
// Here, "does not exist" is a 404 on a typed error, and a long operation is a
// poller rather than a sleep loop.
package azure

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerregistry/armcontainerregistry"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/postgresql/armpostgresqlflexibleservers/v4"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

// Clients is everything this extension talks to, for one subscription.
type Clients struct {
	SubscriptionID string
	ResourceGroup  string

	credential azcore.TokenCredential

	ContainerApps *armappcontainers.ContainerAppsClient
	Revisions     *armappcontainers.ContainerAppsRevisionsClient
	Registries    *armcontainerregistry.RegistriesClient
	Runs          *armcontainerregistry.RunsClient
	Databases     *armpostgresqlflexibleservers.DatabasesClient
}

// New builds the clients for a subscription and resource group.
//
// The credential is deliberately explicit rather than DefaultAzureCredential:
// that chain's silent fallbacks turn a misconfiguration into a confusing
// failure several steps later instead of a clear one here. Inside a container
// the managed identity answers; anywhere else — a laptop, or a runner after
// azure/login — it is the CLI session.
func New(ctx context.Context, subscriptionID, resourceGroup string) (*Clients, error) {
	credential, err := newCredential()
	if err != nil {
		return nil, err
	}

	apps, err := armappcontainers.NewClientFactory(subscriptionID, credential, nil)
	if err != nil {
		return nil, err
	}
	registry, err := armcontainerregistry.NewClientFactory(subscriptionID, credential, nil)
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
		ContainerApps:  apps.NewContainerAppsClient(),
		Revisions:      apps.NewContainerAppsRevisionsClient(),
		Registries:     registry.NewRegistriesClient(),
		Runs:           registry.NewRunsClient(),
		Databases:      postgres.NewDatabasesClient(),
	}, nil
}

func newCredential() (azcore.TokenCredential, error) {
	// Container Apps injects IDENTITY_ENDPOINT and the token comes from that
	// local endpoint — no secret, nothing to rotate.
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

// Credential exposes the shared credential for the few calls that need to build
// their own client, such as uploading a build context to a SAS URL.
func (c *Clients) Credential() azcore.TokenCredential { return c.credential }

// NotFound reports whether an error is Azure saying "no such thing".
//
// The distinction that matters is that a 403 is not a 404. An operator without
// data-plane access to a POPULATED Key Vault must not be told it is empty, or
// the next thing that happens is a seeding run overwriting live secrets.
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
