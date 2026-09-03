// Package config reads what a repository has to say about its previews.
//
// Everything here genuinely varies between repositories. Everything that does
// not — the preview lifecycle, the per-push image tag, the order a release
// happens in — lives in the extension and is deliberately not configurable. A
// second repository doing those differently is how two deployments drift into
// needing two sets of instructions.
//
// Resource NAMES are not configurable either, beyond the project name. They
// come from the deployment outputs, which is the one source that cannot
// disagree with what is actually deployed.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the config a repository puts beside its azure.yaml.
const FileName = "preview.yaml"

// Config is the whole of preview.yaml.
type Config struct {
	// PreviewEnvironment is the environment previews are deployed alongside.
	// They always live in an existing one, never their own: azd's unit is an
	// environment, and one per pull request would mean a resource group,
	// database server and everything else per pull request.
	//
	// Qualified rather than plain `environment`, because this file describes an
	// ordinary release as well, and there "the environment" means the one being
	// deployed.
	PreviewEnvironment string `yaml:"previewEnvironment"`

	// Provision brings a database up to date — migrations, seeds, whatever a
	// release needs before anything can serve. Run with the preview's own
	// database and URL in its environment, on whichever machine is deploying,
	// so it authenticates as the CALLER rather than as the app.
	Provision string `yaml:"provision"`


	// Env is what a preview overrides on the app it was cloned from.
	// Everything else — identity, registry, secret references, probes, every
	// other variable — is inherited, so a new variable in the infrastructure
	// reaches previews without this file being touched.
	//
	// ${url}, ${database} and ${pr} are substituted.
	Env map[string]string `yaml:"env"`

	// ProvisionEnv is the environment the provision command runs with, for both
	// a preview and an ordinary release. It exists because migrating a database
	// needs things a container gets from its own configuration — a host, a
	// login, a secret — and the machine running the deploy has none of them.
	//
	// Substitutions: ${output:NAME} for a deployment output, ${secret:name} for
	// a Key Vault secret, ${env:NAME} for the caller's own environment (with
	// ${env:NAME|fallback}), plus ${url}, ${database} and ${pr}.
	ProvisionEnv map[string]string `yaml:"provisionEnv"`

	// Dockerfile relative to the repository root.
	Dockerfile string `yaml:"dockerfile"`
}

// Load reads preview.yaml from dir, applying defaults.
//
// Returns (nil, nil) when there is no preview.yaml at all. Every other problem is
// an error: a handler that cannot tell "this repo opts out" from "this repo's
// config is broken" will silently skip the migration it was supposed to run.
func Load(dir string) (*Config, error) {
	path := filepath.Join(dir, FileName)

	contents, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Absent is an answer, not a failure: a repository that has this
			// extension installed but does not use it should be left alone.
			// Anything else — unreadable, malformed, missing a required field —
			// is a failure, and must not be mistaken for absence.
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", FileName, err)
	}

	var config Config
	if err := yaml.Unmarshal(contents, &config); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", FileName, err)
	}

	if config.PreviewEnvironment == "" {
		return nil, fmt.Errorf(
			"%s: `previewEnvironment` is required — previews are deployed alongside an existing environment",
			FileName)
	}
	if config.Dockerfile == "" {
		config.Dockerfile = "Dockerfile"
	}

	return &config, nil
}

// Vars is what a substitution can draw on.
type Vars struct {
	PR       int
	URL      string
	Database string
	Outputs  map[string]string
	Secret   func(name string) (string, error)
}

var reference = regexp.MustCompile(`\$\{([a-z]+):([^}|]+)(?:\|([^}]*))?\}`)

// Expand substitutes ${url}, ${database} and ${pr}.
func Expand(values map[string]string, vars Vars) map[string]string {
	replacer := strings.NewReplacer(
		"${url}", vars.URL,
		"${database}", vars.Database,
		"${pr}", fmt.Sprint(vars.PR),
	)

	expanded := make(map[string]string, len(values))
	for key, value := range values {
		expanded[key] = replacer.Replace(value)
	}
	return expanded
}

// ExpandFull also resolves ${output:NAME}, ${env:NAME|fallback} and
// ${secret:name}. Secrets are fetched lazily, so a config naming none costs no
// round trip.
func ExpandFull(values map[string]string, vars Vars) (map[string]string, error) {
	expanded := Expand(values, vars)

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
				if fallback != "" {
					return fallback
				}
				failure = fmt.Errorf(
					"%s wants ${output:%s}, which the deployment did not produce", key, name)
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

