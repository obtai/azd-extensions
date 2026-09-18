package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"

	"github.com/obtai/azd-extensions/preview/internal/azure"
	"github.com/obtai/azd-extensions/preview/internal/config"
	"github.com/obtai/azd-extensions/preview/internal/preview"
)

// outputs reads an azd environment's deployment outputs, which azd writes
// uppercased from the Bicep outputs.
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

// environmentPrefix: `sales` gives SALES_CONTAINER_APP_NAME and so on.
func environmentPrefix(project string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(project))
}

// resolver answers "what is the value of X" in the one order that matters:
// what preview.yaml says explicitly, then the azd environment by the
// conventional name, then the process environment by the same name. The last
// is what lets a repository with no azd environment run this at all.
type resolver struct {
	values      map[string]string
	environment string
	vars        config.Vars
}

func (r resolver) optional(explicit, field, conventional string) (string, error) {
	if explicit != "" {
		resolved, err := config.ExpandOne(field, explicit, r.vars)
		if err != nil {
			return "", err
		}
		if resolved == "" {
			return "", fmt.Errorf("%s resolved to nothing", field)
		}
		return resolved, nil
	}
	if value := r.values[conventional]; value != "" {
		return value, nil
	}
	return os.Getenv(conventional), nil
}

func (r resolver) required(explicit, field, conventional string) (string, error) {
	value, err := r.optional(explicit, field, conventional)
	if err != nil {
		return "", err
	}
	if value != "" {
		return value, nil
	}
	if r.environment != "" {
		return "", fmt.Errorf(
			"%s is not set — set it in the azd environment (`azd env refresh -e %s`), export it, "+
				"or name it under `outputs:` in %s", conventional, r.environment, config.FileName)
	}
	return "", fmt.Errorf(
		"%s is not set — export it, or name it under `outputs:` in %s "+
			"(there is no previewEnvironment to read it from)", conventional, config.FileName)
}

// newTarget assembles everything a preview needs, from the environment previews
// live alongside — or, without one, from what the caller exported.
func newTarget(ctx context.Context, client *azdext.AzdClient, log io.Writer) (*preview.Target, error) {
	settings, err := config.Load(".")
	if err != nil {
		return nil, err
	}
	if settings == nil {
		return nil, fmt.Errorf(
			"no %s here — it names the environment previews live alongside, the command "+
				"that migrates them, and the variables they override", config.FileName)
	}

	// The project name is the default prefix, the default image repository and
	// the default service name. A repository with no azure.yaml can still run
	// previews when it names everything itself; it is called `web` then.
	name := "web"
	if project, err := client.Project().Get(ctx, &azdext.EmptyRequest{}); err == nil {
		name = project.Project.Name
	} else if settings.PreviewEnvironment != "" {
		return nil, fmt.Errorf("no azd project here: %w", err)
	}

	values := map[string]string{}
	if settings.PreviewEnvironment != "" {
		values, err = outputs(ctx, client, settings.PreviewEnvironment)
		if err != nil {
			return nil, err
		}
	}

	prefix := settings.Prefix
	if prefix == "" {
		prefix = environmentPrefix(name)
	}

	resolve := resolver{
		values:      values,
		environment: settings.PreviewEnvironment,
		vars:        config.Vars{Outputs: values},
	}

	subscription, err := resolve.required(settings.Outputs.SubscriptionID, "outputs.subscriptionId", "AZURE_SUBSCRIPTION_ID")
	if err != nil {
		return nil, err
	}
	group, err := resolve.required(settings.Outputs.ResourceGroup, "outputs.resourceGroup", "AZURE_RESOURCE_GROUP")
	if err != nil {
		return nil, err
	}
	domain, err := resolve.required(settings.Outputs.Domain, "outputs.domain", prefix+"_CONTAINER_APPS_ENV_DOMAIN")
	if err != nil {
		return nil, err
	}
	// The registry's ARM resource name is not needed: building, pushing,
	// listing and deleting tags all go through the login server.
	login, err := resolve.required(settings.Outputs.Registry, "outputs.registry", "AZURE_CONTAINER_REGISTRY_ENDPOINT")
	if err != nil {
		return nil, err
	}
	server, err := resolve.optional(settings.Outputs.PostgresServer, "outputs.postgresServer", prefix+"_POSTGRES_SERVER_NAME")
	if err != nil {
		return nil, err
	}
	if settings.Database != nil && settings.Database.Server != "" {
		server, err = resolve.optional(settings.Database.Server, "database.server", "")
		if err != nil {
			return nil, err
		}
	}
	vault, err := resolve.optional(settings.Outputs.KeyVault, "outputs.keyVault", prefix+"_KEY_VAULT_NAME")
	if err != nil {
		return nil, err
	}

	services, err := resolveServices(settings, resolve, name, prefix)
	if err != nil {
		return nil, err
	}

	clients, err := azure.New(ctx, subscription, group)
	if err != nil {
		return nil, err
	}
	clients.Log = log

	return &preview.Target{
		Clients:  clients,
		Config:   settings,
		Project:  name,
		Services: services,
		Domain:   domain,
		Login:    login,
		Server:   server,
		Vault:    vault,
		Outputs:  values,
		Log:      log,
	}, nil
}

// resolveServices names each service's apps.
//
// The shorthand is 0.5.0's shape: the service app is <PREFIX>_CONTAINER_APP_NAME
// and `previewApp`, when set, is a dedicated app beside it. Under `services`
// there is no second app: `app` is the one azd deploys, defaulting to
// <PREFIX>_<SERVICE>_CONTAINER_APP_NAME, and previews are revisions of it.
func resolveServices(
	settings *config.Config, resolve resolver, project, prefix string,
) ([]preview.Service, error) {
	var services []preview.Service
	seen := map[string]string{}
	for _, declared := range settings.ServiceList(project) {
		var serviceApp, previewApp string
		var err error
		if settings.Shorthand() {
			serviceApp, err = resolve.required("", "", prefix+"_CONTAINER_APP_NAME")
			if err != nil {
				return nil, err
			}
			// Which app previews land on. Defaults to the one azd deploys, which
			// is the shape that needs the template restoring afterwards — see the
			// preview package comment. A repository that declares an app for
			// previews names it here and gets the simpler path.
			previewApp = serviceApp
			if declared.App != "" {
				previewApp, err = resolve.required(declared.App, "previewApp", "")
				if err != nil {
					return nil, err
				}
			}
		} else {
			conventional := prefix + "_" + environmentPrefix(declared.Name) + "_CONTAINER_APP_NAME"
			serviceApp, err = resolve.required(declared.App, "services."+declared.Name+".app", conventional)
			if err != nil {
				return nil, err
			}
			previewApp = serviceApp
		}
		if other, taken := seen[previewApp]; taken {
			return nil, fmt.Errorf("services %s and %s both preview on %s", other, declared.Name, previewApp)
		}
		seen[previewApp] = declared.Name
		services = append(services, preview.Service{
			Name:       declared.Name,
			ServiceApp: serviceApp,
			PreviewApp: previewApp,
			Config:     declared.Service,
		})
	}
	return services, nil
}
