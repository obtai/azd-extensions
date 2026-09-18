// Package config reads what a repository has to say about its previews.
//
// Only what genuinely varies between repositories belongs here — the preview
// lifecycle and resource names are deliberately not configurable.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the config a repository puts beside its azure.yaml.
const FileName = "preview.yaml"

// DefaultHealthPath is what a service is probed at unless it says otherwise.
const DefaultHealthPath = "/api/health"

// Config is the whole of preview.yaml.
type Config struct {
	// PreviewEnvironment is the azd environment previews are deployed alongside.
	// They always live in an existing one, never their own: azd's unit is an
	// environment, and one per pull request would mean a resource group,
	// database server and everything else per pull request.
	//
	// Optional since 0.6.0. Without it no azd environment is read at all, and
	// every value comes from `outputs:` or the process environment — which is
	// how a repository whose infrastructure was not provisioned by azd runs
	// previews.
	//
	// Qualified rather than plain `environment`, because this file describes an
	// ordinary release as well, and there "the environment" means the one being
	// deployed.
	PreviewEnvironment string `yaml:"previewEnvironment"`

	// Prefix is the deployment-output prefix: `SALES` gives
	// SALES_CONTAINER_APP_NAME, SALES_CONTAINER_APPS_ENV_DOMAIN and so on.
	// Defaults to the azd project name uppercased, with dashes and dots as
	// underscores.
	Prefix string `yaml:"prefix"`

	// Outputs names the deployment values explicitly, for a deployment whose
	// outputs do not follow the prefix convention. Each accepts ${output:NAME}
	// and ${env:NAME}. Anything left unset is looked up by its conventional
	// name: in the azd environment first, then the process environment.
	Outputs Outputs `yaml:"outputs"`

	// Provision brings a database up to date — migrations, seeds, whatever a
	// release needs before anything can serve. Run with the preview's own
	// database and URL in its environment, on whichever machine is deploying,
	// so it authenticates as the CALLER rather than as the app.
	Provision string `yaml:"provision"`

	// Env is what a preview overrides on the revision production is serving.
	// Everything else — identity, registry, secret references, probes, every
	// other variable — is inherited, so a new variable in the infrastructure
	// reaches previews without this file being touched.
	//
	// The single-service shorthand for `services.<name>.env`; the two may not
	// be combined.
	//
	// Substitutions: ${url}, ${database}, ${label}, ${pr}, ${url:<service>},
	// ${fqdn:<service>}, ${output:NAME} and ${env:NAME|fallback}. Not
	// ${secret:name}: a revision template is stored in plaintext, so a secret
	// belongs in a Key Vault reference the app already carries.
	Env map[string]string `yaml:"env"`

	// ProvisionEnv is extra environment for the provision command, on top of
	// the deployment outputs it already receives. Reach for it when a migration
	// needs something the deployment did not record — a Key Vault secret, or a
	// value that differs per caller.
	//
	// Substitutions: ${output:NAME} for a deployment output, ${secret:name} for
	// a Key Vault secret, ${env:NAME} for the caller's own environment (with
	// ${env:NAME|fallback}), plus ${url}, ${database}, ${label}, ${pr},
	// ${url:<service>} and ${fqdn:<service>}.
	ProvisionEnv map[string]string `yaml:"provisionEnv"`

	// PreviewApp is the container app preview revisions are added to. Defaults
	// to the app the azd service deploys. The single-service shorthand for
	// `services.<name>.app`.
	//
	// Prefer naming a DEDICATED app here — one the repository's own
	// infrastructure declares and nothing else deploys to. A revision is minted
	// from its app's template, so a preview necessarily writes its own APP_ENV
	// and database name onto whichever app hosts it. On an app of its own that
	// is harmless. On the app serving production it is not, and the extension
	// has to put the template back afterwards, minting a revision every push to
	// do it.
	//
	// Substitutions: ${output:NAME} for a deployment output, ${env:NAME}, and
	// ${secret:name}.
	PreviewApp string `yaml:"previewApp"`

	// LiveLabel is the revision label carrying production traffic on the app
	// previews are added to. Only consulted when previews share the service app,
	// which is the shape PreviewApp above exists to avoid. Defaults to `live`.
	//
	// A preview is a zero-traffic revision of that app, so the extension has to
	// know which revision production is actually on: it is the template a
	// preview is derived from, and the one the app is restored to afterwards.
	// Naming it beats reading the traffic split, because a label says which
	// revision is deliberate where a weight of 100 says only which one won.
	// When no revision carries the label, the one holding 100% of traffic is
	// used instead.
	LiveLabel string `yaml:"liveLabel"`

	// Dockerfile relative to the repository root. The single-service shorthand
	// for `services.<name>.dockerfile`.
	Dockerfile string `yaml:"dockerfile"`

	// Labels is a fixed pool of revision labels previews borrow from, in
	// preference order. A label's URL is stable for as long as the label
	// exists, and an app that signs people in through Entra has to register
	// every callback URL by hand — so a pool of N names registered once beats
	// a `pr-<n>` per pull request registered never. A pull request keeps the
	// label it holds across pushes and gives it back on `down`; when every
	// label is held, `up` fails rather than queueing. Unset, the label is
	// `pr-<n>`.
	Labels []string `yaml:"labels"`

	// Database says whether the extension creates the pull request's database
	// itself, and where.
	Database *Database `yaml:"database"`

	// Services declares more than one container app per pull request, keyed by
	// azd service name and minted in declaration order. Every service's URL is
	// known before anything is built, so one service's env can name another's
	// with ${url:<service>} or ${fqdn:<service>}. Leave it out for a
	// single-service repository: `previewApp`, `dockerfile` and `env` at the
	// top level describe one service named after the azd project.
	Services Services `yaml:"services"`
}

// Outputs are the deployment values a preview needs, named explicitly.
type Outputs struct {
	// SubscriptionID, conventionally AZURE_SUBSCRIPTION_ID.
	SubscriptionID string `yaml:"subscriptionId"`
	// ResourceGroup, conventionally AZURE_RESOURCE_GROUP.
	ResourceGroup string `yaml:"resourceGroup"`
	// Registry is the login server, conventionally
	// AZURE_CONTAINER_REGISTRY_ENDPOINT.
	Registry string `yaml:"registry"`
	// Domain is the Container Apps environment's default domain, conventionally
	// <PREFIX>_CONTAINER_APPS_ENV_DOMAIN.
	Domain string `yaml:"domain"`
	// PostgresServer is the flexible server's name, conventionally
	// <PREFIX>_POSTGRES_SERVER_NAME. Optional: without one there is no
	// database to create or drop.
	PostgresServer string `yaml:"postgresServer"`
	// KeyVault is the vault ${secret:name} reads, conventionally
	// <PREFIX>_KEY_VAULT_NAME. Optional.
	KeyVault string `yaml:"keyVault"`
}

// Database is what the extension does about the pull request's database.
type Database struct {
	// Create makes the extension create the database over ARM before
	// `provision` runs, rather than leaving it to that command. A server behind
	// a private endpoint cannot be reached from a runner with psql, and an app
	// that migrates itself at boot has no provision step at all — ARM is the
	// one door that is always open.
	Create bool `yaml:"create"`
	// Server overrides the flexible server's name. Accepts ${output:NAME} and
	// ${env:NAME}.
	Server string `yaml:"server"`
	// Name overrides the database name, which is `<project>_pr_<n>` by
	// default. Accepts ${pr} and ${label}.
	Name string `yaml:"name"`
}

// Service is one container app of a pull request's preview.
type Service struct {
	// App is the container app this service's preview revisions are added to.
	// Defaults to the service's own deployment output,
	// <PREFIX>_<SERVICE>_CONTAINER_APP_NAME. Accepts ${output:NAME} and
	// ${env:NAME}.
	App string `yaml:"app"`
	// Dockerfile relative to the repository root. Defaults to `Dockerfile`.
	Dockerfile string `yaml:"dockerfile"`
	// Context is the build context, relative to the repository root. Defaults
	// to `.`.
	Context string `yaml:"context"`
	// Image is the registry repository the build is pushed to. Defaults to the
	// service name.
	Image string `yaml:"image"`
	// Container names which container in the app's template gets the new
	// image and the env overrides. Defaults to the first.
	Container string `yaml:"container"`
	// Env is what the preview overrides on this service's template. The same
	// substitutions as the top-level `env`.
	Env map[string]string `yaml:"env"`
	// HealthPath is probed on the preview URL once the revision is healthy.
	// Defaults to `/api/health`.
	HealthPath string `yaml:"healthPath"`
	// Scale bounds the preview's replicas. Defaults to 0–1, so an idle preview
	// costs nothing.
	Scale *Scale `yaml:"scale"`
}

// Scale is a replica range.
type Scale struct {
	Min *int32 `yaml:"min"`
	Max *int32 `yaml:"max"`
}

// NamedService is a Service with its key, in the order it was declared.
type NamedService struct {
	Name string
	Service
}

// Services is an ordered list decoded from a YAML mapping. Order matters — it
// is the build and mint order, and a map would lose it.
type Services []NamedService

// UnmarshalYAML keeps the mapping's order. Each value is decoded strictly, the
// way the top level is, so a mistyped key inside a service is still an error.
func (s *Services) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: services must be a mapping of service name to settings", node.Line)
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if seen[key.Value] {
			return fmt.Errorf("line %d: service %q is declared twice", key.Line, key.Value)
		}
		seen[key.Value] = true

		var service Service
		if err := strict(value, &service); err != nil {
			return fmt.Errorf("services.%s: %w", key.Value, err)
		}
		*s = append(*s, NamedService{Name: key.Value, Service: service})
	}
	return nil
}

// strict decodes a node with unknown keys rejected. Node.Decode cannot be told
// to, so the node goes back through a Decoder that can.
func strict(node *yaml.Node, out any) error {
	encoded, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(encoded))
	decoder.KnownFields(true)
	if err := decoder.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// Load reads preview.yaml from dir, applying defaults. Returns (nil, nil) when
// there is no preview.yaml; every other problem is an error.
func Load(dir string) (*Config, error) {
	path := filepath.Join(dir, FileName)

	contents, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", FileName, err)
	}
	return Parse(contents)
}

// Parse reads a preview.yaml's contents, applying defaults and checking what
// can be checked without Azure.
func Parse(contents []byte) (*Config, error) {
	// KnownFields, so a mistyped key is an error naming the key rather than a
	// line that is silently ignored.
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)

	var config Config
	if err := decoder.Decode(&config); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing %s: %w", FileName, err)
	}

	if config.Dockerfile == "" {
		config.Dockerfile = "Dockerfile"
	}
	if config.LiveLabel == "" {
		config.LiveLabel = "live"
	}

	if len(config.Services) > 0 {
		for _, key := range []struct {
			set  bool
			name string
		}{
			{config.PreviewApp != "", "previewApp"},
			{config.Dockerfile != "Dockerfile", "dockerfile"},
			{len(config.Env) > 0, "env"},
		} {
			if key.set {
				return nil, fmt.Errorf(
					"%s: `%s` is the single-service shorthand — with `services`, set it per service",
					FileName, key.name)
			}
		}
	}
	for i := range config.Services {
		applyServiceDefaults(&config.Services[i].Service, config.Services[i].Name)
	}

	if err := rejectSecrets("env", config.Env); err != nil {
		return nil, err
	}
	for _, service := range config.Services {
		if err := rejectSecrets("services."+service.Name+".env", service.Env); err != nil {
			return nil, err
		}
	}

	seen := map[string]bool{}
	for _, label := range config.Labels {
		if err := ValidateLabel(label); err != nil {
			return nil, fmt.Errorf("%s: labels: %w", FileName, err)
		}
		if seen[label] {
			return nil, fmt.Errorf("%s: labels: %q is listed twice", FileName, label)
		}
		seen[label] = true
	}

	return &config, nil
}

func applyServiceDefaults(service *Service, name string) {
	if service.Dockerfile == "" {
		service.Dockerfile = "Dockerfile"
	}
	if service.Context == "" {
		service.Context = "."
	}
	if service.Image == "" {
		service.Image = name
	}
	if service.HealthPath == "" {
		service.HealthPath = DefaultHealthPath
	}
}

// ServiceList is the services to preview, in order. The single-service
// shorthand becomes one service named after the project, whose image
// repository is the project name — which is what every 0.5.0 consumer
// pushes to.
func (c *Config) ServiceList(project string) []NamedService {
	if len(c.Services) > 0 {
		return c.Services
	}
	service := Service{
		App:        c.PreviewApp,
		Dockerfile: c.Dockerfile,
		Env:        c.Env,
		Image:      project,
	}
	applyServiceDefaults(&service, project)
	return []NamedService{{Name: project, Service: service}}
}

// Shorthand reports whether the file uses the single-service form, where the
// service app comes from <PREFIX>_CONTAINER_APP_NAME rather than a name per
// service.
func (c *Config) Shorthand() bool { return len(c.Services) == 0 }

// A revision label may not contain two consecutive dashes and is capped at 64
// characters. A revision name — <app>--<suffix> — is capped at 64 too.
var labelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ValidateLabel is what Container Apps accepts as a traffic label.
func ValidateLabel(label string) error {
	if len(label) > 64 || strings.Contains(label, "--") || !labelPattern.MatchString(label) {
		return fmt.Errorf("invalid revision label %q", label)
	}
	return nil
}

var secretReference = regexp.MustCompile(`\$\{secret:[^}]*\}`)

func rejectSecrets(where string, values map[string]string) error {
	for key, value := range values {
		if secretReference.MatchString(value) {
			return fmt.Errorf(
				"%s: %s.%s uses ${secret:…}, but a revision template is plaintext — "+
					"reference the secret from the app's template instead, or use provisionEnv",
				FileName, where, key)
		}
	}
	return nil
}

// Vars is what a substitution can draw on.
type Vars struct {
	PR       int
	URL      string
	Database string
	Label    string
	// URLs is every service's preview URL by service name, for ${url:name}
	// and ${fqdn:name}.
	URLs    map[string]string
	Outputs map[string]string
	Secret  func(name string) (string, error)
}

var reference = regexp.MustCompile(`\$\{([a-z]+):([^}|]+)(?:\|([^}]*))?\}`)

// Expand substitutes ${url}, ${database}, ${label}, ${pr}, ${url:service} and
// ${fqdn:service}. A service that does not exist is an error, because the
// empty string it would otherwise become is a working URL to nowhere.
func Expand(values map[string]string, vars Vars) (map[string]string, error) {
	replacer := strings.NewReplacer(
		"${url}", vars.URL,
		"${database}", vars.Database,
		"${label}", vars.Label,
		"${pr}", fmt.Sprint(vars.PR),
	)

	expanded := make(map[string]string, len(values))
	for key, value := range values {
		var failure error
		resolved := reference.ReplaceAllStringFunc(replacer.Replace(value), func(match string) string {
			parts := reference.FindStringSubmatch(match)
			kind, name := parts[1], parts[2]
			if kind != "url" && kind != "fqdn" {
				return match
			}
			url, ok := vars.URLs[name]
			if !ok {
				failure = fmt.Errorf("%s wants ${%s:%s}, but there is no service %q", key, kind, name, name)
				return ""
			}
			if kind == "fqdn" {
				return strings.TrimPrefix(url, "https://")
			}
			return url
		})
		if failure != nil {
			return nil, failure
		}
		expanded[key] = resolved
	}
	return expanded, nil
}

// ExpandFull also resolves ${output:NAME}, ${env:NAME|fallback} and
// ${secret:name}.
//
// An output that the azd environment did not produce is read from the process
// environment before giving up, so a repository with no azd environment can
// export what it needs under the same names.
func ExpandFull(values map[string]string, vars Vars) (map[string]string, error) {
	expanded, err := Expand(values, vars)
	if err != nil {
		return nil, err
	}

	for key, value := range expanded {
		var failure error

		resolved := reference.ReplaceAllStringFunc(value, func(match string) string {
			parts := reference.FindStringSubmatch(match)
			kind, name, fallback := parts[1], parts[2], parts[3]

			switch kind {
			case "output":
				if found, ok := vars.Outputs[name]; ok && found != "" {
					return found
				}
				if found := os.Getenv(name); found != "" {
					return found
				}
				if fallback != "" {
					return fallback
				}
				failure = fmt.Errorf(
					"%s wants ${output:%s}, which the deployment did not produce — "+
						"set it in the azd environment or export it", key, name)
			case "env":
				if found := os.Getenv(name); found != "" {
					return found
				}
				return fallback
			case "secret":
				if vars.Secret == nil {
					failure = fmt.Errorf("%s wants ${secret:%s} but no vault is available", key, name)
					return ""
				}
				found, err := vars.Secret(name)
				if err != nil {
					failure = fmt.Errorf("%s wants ${secret:%s}: %w", key, name, err)
					return ""
				}
				return found
			default:
				failure = fmt.Errorf("%s: unknown substitution %q", key, kind)
			}
			return ""
		})

		if failure != nil {
			return nil, failure
		}
		expanded[key] = resolved
	}

	return expanded, nil
}

// ExpandOne resolves a single value with every substitution.
func ExpandOne(name, value string, vars Vars) (string, error) {
	resolved, err := ExpandFull(map[string]string{name: value}, vars)
	if err != nil {
		return "", err
	}
	return resolved[name], nil
}
