package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"

	"obtai.preview/internal/azure"
	"obtai.preview/internal/config"
	"obtai.preview/internal/preview"
)

// outputs reads an azd environment's deployment outputs.
//
// azd writes every Bicep output into the environment, uppercased. Nothing in
// this extension re-derives a resource name from a convention: registries, Key
// Vaults, database servers and AI accounts carry a uniqueString() suffix and
// are not guessable, and the alternatives — listing and taking the first, or
// parsing a parameter file — can all disagree with what is actually deployed.
// The deployment cannot.
func outputs(ctx context.Context, client *azdext.AzdClient, name string) (map[string]string, error) {
	response, err := client.Environment().GetValues(ctx, &azdext.GetEnvironmentRequest{Name: name})
	if err != nil {
		return nil, fmt.Errorf("reading environment %q: %w", name, err)
	}

	values := make(map[string]string, len(response.KeyValues))
	for _, pair := range response.KeyValues {
		values[pair.Key] = pair.Value
	}
	return values, nil
}

// environmentPrefix is the prefix a Bicep template following this convention
// emits: `sales` gives SALES_CONTAINER_APP_NAME, SALES_KEY_VAULT_NAME and so on.
func environmentPrefix(project string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(project))
}

func require(values map[string]string, key, environment string) (string, error) {
	if value := values[key]; value != "" {
		return value, nil
	}
	return "", fmt.Errorf("%s is not set — run `azd env refresh -e %s`", key, environment)
}

// newTarget assembles everything a preview needs, from the environment previews
// live alongside.
func newTarget(ctx context.Context, client *azdext.AzdClient) (*preview.Target, error) {
	settings, err := config.Load(".")
	if err != nil {
		return nil, err
	}
	if settings == nil {
		return nil, fmt.Errorf(
			"no %s here — it names the environment previews live alongside, the command "+
				"that migrates them, and the variables they override", config.FileName)
	}

	project, err := client.Project().Get(ctx, &azdext.EmptyRequest{})
	if err != nil {
		return nil, fmt.Errorf("no azd project here: %w", err)
	}

	return targetFor(ctx, client, project.Project.Name, settings.PreviewEnvironment)
}

// targetFor assembles a target against a named environment. A preview always
// uses the one named in preview.yaml; a release uses whichever is selected.
func targetFor(
	ctx context.Context,
	client *azdext.AzdClient,
	name string,
	environment string,
) (*preview.Target, error) {
	settings, err := config.Load(".")
	if err != nil {
		return nil, err
	}
	if settings == nil {
		return nil, fmt.Errorf("no %s here", config.FileName)
	}

	values, err := outputs(ctx, client, environment)
	if err != nil {
		return nil, err
	}

	prefix := environmentPrefix(name)

	subscription, err := require(values, "AZURE_SUBSCRIPTION_ID", environment)
	if err != nil {
		return nil, err
	}
	group, err := require(values, "AZURE_RESOURCE_GROUP", environment)
	if err != nil {
		return nil, err
	}
	sourceApp, err := require(values, prefix+"_CONTAINER_APP_NAME", environment)
	if err != nil {
		return nil, err
	}
	domain, err := require(values, prefix+"_CONTAINER_APPS_ENV_DOMAIN", environment)
	if err != nil {
		return nil, err
	}
	login, err := require(values, "AZURE_CONTAINER_REGISTRY_ENDPOINT", environment)
	if err != nil {
		return nil, err
	}
	// Scheduling a build is a management-plane call and wants the registry's
	// resource name; listing and deleting tags are data-plane and want the
	// login server.
	registry, err := require(values, prefix+"_ACR_NAME", environment)
	if err != nil {
		return nil, err
	}

	clients, err := azure.New(ctx, subscription, group)
	if err != nil {
		return nil, err
	}

	return &preview.Target{
		Clients:   clients,
		Config:    settings,
		Project:   name,
		SourceApp: sourceApp,
		Domain:    domain,
		Registry:  registry,
		Login:     login,
		Server:    values[prefix+"_POSTGRES_SERVER_NAME"],
		Vault:     values[prefix+"_KEY_VAULT_NAME"],
		Outputs:   values,
	}, nil
}
