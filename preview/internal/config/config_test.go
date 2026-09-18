package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

func load(t *testing.T, fixture string) *Config {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	config, err := Parse(contents)
	if err != nil {
		t.Fatalf("parsing %s: %v", fixture, err)
	}
	return config
}

// The sales file is a 0.5.0 consumer, copied byte for byte. It has to load
// with every 0.5.0 value intact and every 0.6.0 addition unset.
func TestLoadsSalesUnchanged(t *testing.T) {
	config := load(t, "sales.preview.yaml")

	if config.PreviewEnvironment != "prod" {
		t.Errorf("PreviewEnvironment = %q", config.PreviewEnvironment)
	}
	if config.Provision != "sh scripts/hooks/predeploy.sh" {
		t.Errorf("Provision = %q", config.Provision)
	}
	if config.PreviewApp != "${output:SALES_PREVIEW_APP_NAME}" {
		t.Errorf("PreviewApp = %q", config.PreviewApp)
	}
	want := map[string]string{
		"APP_ENV":         "preview",
		"APP_URL":         "${url}",
		"BETTER_AUTH_URL": "${url}",
		"PG_DATABASE":     "${database}",
		"PG_POOL_MAX":     "5",
	}
	if !reflect.DeepEqual(config.Env, want) {
		t.Errorf("Env = %v, want %v", config.Env, want)
	}
	// Defaults, as before.
	if config.Dockerfile != "Dockerfile" || config.LiveLabel != "live" {
		t.Errorf("Dockerfile = %q, LiveLabel = %q", config.Dockerfile, config.LiveLabel)
	}
	// Nothing new switched on by accident.
	if config.Prefix != "" || len(config.Labels) != 0 || config.Database != nil ||
		len(config.Services) != 0 || config.Outputs != (Outputs{}) {
		t.Errorf("0.6.0 fields set on a 0.5.0 file: %+v", config)
	}
	if !config.Shorthand() {
		t.Error("the sales file is the single-service shorthand")
	}
}

// The shorthand is one service named after the project, pushing to the
// project's repository — exactly what 0.5.0 did.
func TestShorthandBecomesOneService(t *testing.T) {
	config := load(t, "sales.preview.yaml")

	services := config.ServiceList("sales")
	if len(services) != 1 {
		t.Fatalf("got %d services, want 1", len(services))
	}
	service := services[0]
	if service.Name != "sales" || service.Image != "sales" {
		t.Errorf("Name = %q, Image = %q, want sales", service.Name, service.Image)
	}
	if service.App != "${output:SALES_PREVIEW_APP_NAME}" {
		t.Errorf("App = %q, want previewApp carried over", service.App)
	}
	if service.Dockerfile != "Dockerfile" || service.Context != "." {
		t.Errorf("Dockerfile = %q, Context = %q", service.Dockerfile, service.Context)
	}
	if service.HealthPath != DefaultHealthPath {
		t.Errorf("HealthPath = %q, want %s", service.HealthPath, DefaultHealthPath)
	}
	if service.Env["PG_DATABASE"] != "${database}" {
		t.Errorf("Env = %v, want the top-level env", service.Env)
	}
}

func TestLoadsPlot(t *testing.T) {
	config := load(t, "plot.preview.yaml")

	if config.Prefix != "APP" {
		t.Errorf("Prefix = %q", config.Prefix)
	}
	if len(config.Labels) != 10 || config.Labels[0] != "comet" || config.Labels[9] != "quartz" {
		t.Errorf("Labels = %v", config.Labels)
	}
	if config.Database == nil || !config.Database.Create ||
		config.Database.Server != "${output:APP_DATABASE_SERVER_NAME}" {
		t.Errorf("Database = %+v", config.Database)
	}

	// Declaration order, not map order: api builds and mints before ui.
	if len(config.Services) != 2 || config.Services[0].Name != "api" || config.Services[1].Name != "ui" {
		t.Fatalf("Services = %v", config.Services)
	}
	api, ui := config.Services[0], config.Services[1]
	if api.Dockerfile != "apps/api/Dockerfile" || api.Context != "." || api.HealthPath != "/api/health" {
		t.Errorf("api = %+v", api.Service)
	}
	if api.Image != "api" || ui.Image != "ui" {
		t.Errorf("images = %q, %q — want the service names", api.Image, ui.Image)
	}
	if ui.HealthPath != "/healthz" {
		t.Errorf("ui.HealthPath = %q", ui.HealthPath)
	}
	if ui.Env["API_UPSTREAM"] != "https://${fqdn:api}" {
		t.Errorf("ui.Env = %v", ui.Env)
	}
	if config.Shorthand() {
		t.Error("a file with services is not the shorthand")
	}
	if got := config.ServiceList("plot"); len(got) != 2 || got[0].Name != "api" {
		t.Errorf("ServiceList = %v", got)
	}
}

// A YAML mapping is unordered in most decoders. This one must not be, and the
// order must survive a declaration that would sort differently.
func TestServicesKeepDeclarationOrder(t *testing.T) {
	config, err := Parse([]byte("services:\n  zebra: {}\n  apple: {}\n  mango: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, service := range config.Services {
		names = append(names, service.Name)
	}
	if !reflect.DeepEqual(names, []string{"zebra", "apple", "mango"}) {
		t.Errorf("order = %v, want as declared", names)
	}
}

func TestServicesRejectUnknownKeys(t *testing.T) {
	_, err := Parse([]byte("services:\n  api:\n    dockerfil: x\n"))
	if err == nil || !strings.Contains(err.Error(), "dockerfil") {
		t.Errorf("err = %v, want the mistyped key named", err)
	}
}

func TestServicesRejectTheShorthandAlongside(t *testing.T) {
	for _, contents := range []string{
		"services:\n  api: {}\nenv:\n  A: b\n",
		"services:\n  api: {}\npreviewApp: x\n",
		"services:\n  api: {}\ndockerfile: x\n",
	} {
		if _, err := Parse([]byte(contents)); err == nil {
			t.Errorf("accepted:\n%s", contents)
		}
	}
}

func TestPreviewEnvironmentIsOptional(t *testing.T) {
	config, err := Parse([]byte("previewApp: ca-x\n"))
	if err != nil {
		t.Fatalf("a file without previewEnvironment was refused: %v", err)
	}
	if config.PreviewEnvironment != "" {
		t.Errorf("PreviewEnvironment = %q", config.PreviewEnvironment)
	}
}

func TestLabelsAreValidated(t *testing.T) {
	for _, contents := range []string{
		"labels: [comet, comet]\n",
		"labels: [Comet]\n",
		"labels: [a--b]\n",
		"labels: [-x]\n",
	} {
		if _, err := Parse([]byte(contents)); err == nil {
			t.Errorf("accepted:\n%s", contents)
		}
	}
}

// A revision template is plaintext in the portal and in every GET, so a secret
// substituted into env would be published. provisionEnv is where secrets go.
func TestSecretInEnvIsRefused(t *testing.T) {
	_, err := Parse([]byte("env:\n  TOKEN: ${secret:github-token}\n"))
	if err == nil || !strings.Contains(err.Error(), "plaintext") {
		t.Errorf("err = %v, want a refusal that says why", err)
	}
	_, err = Parse([]byte("services:\n  api:\n    env:\n      TOKEN: ${secret:x}\n"))
	if err == nil || !strings.Contains(err.Error(), "services.api.env") {
		t.Errorf("err = %v, want the service named", err)
	}
	// provisionEnv keeps them.
	if _, err := Parse([]byte("provisionEnv:\n  TOKEN: ${secret:x}\n")); err != nil {
		t.Errorf("provisionEnv secret refused: %v", err)
	}
}

// ── Substitutions ────────────────────────────────────────────────────────

func plotVars() Vars {
	return Vars{
		PR:       7,
		URL:      "https://ca-plot-api---comet.example.io",
		Database: "plot_pr_7",
		Label:    "comet",
		URLs: map[string]string{
			"api": "https://ca-plot-api---comet.example.io",
			"ui":  "https://ca-plot-ui---comet.example.io",
		},
		Outputs: map[string]string{"APP_KEY_VAULT_NAME": "kv-plot"},
	}
}

func TestExpand(t *testing.T) {
	got, err := Expand(map[string]string{
		"URL":      "${url}",
		"DB":       "${database}",
		"LABEL":    "${label}",
		"PR":       "pr-${pr}",
		"ORIGIN":   "${url:ui}",
		"UPSTREAM": "https://${fqdn:api}",
		"OUTPUT":   "${output:APP_KEY_VAULT_NAME}",
	}, plotVars())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"URL":      "https://ca-plot-api---comet.example.io",
		"DB":       "plot_pr_7",
		"LABEL":    "comet",
		"PR":       "pr-7",
		"ORIGIN":   "https://ca-plot-ui---comet.example.io",
		"UPSTREAM": "https://ca-plot-api---comet.example.io",
		// Left for ExpandFull.
		"OUTPUT": "${output:APP_KEY_VAULT_NAME}",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Expand = %v, want %v", got, want)
	}
}

// An unknown service must not become an empty string, which is a working URL
// to nowhere.
func TestExpandRefusesAnUnknownService(t *testing.T) {
	_, err := Expand(map[string]string{"X": "${url:worker}"}, plotVars())
	if err == nil || !strings.Contains(err.Error(), "worker") {
		t.Errorf("err = %v", err)
	}
}

func TestExpandFull(t *testing.T) {
	t.Setenv("CALLER_VALUE", "from-shell")
	t.Setenv("APP_EXPORTED", "exported")
	t.Setenv("APP_KEY_VAULT_NAME", "kv-from-shell")

	vars := plotVars()
	vars.Secret = func(name string) (string, error) { return "secret:" + name, nil }

	got, err := ExpandFull(map[string]string{
		"OUTPUT":       "${output:APP_KEY_VAULT_NAME}",
		"OUTPUT_ENV":   "${output:APP_EXPORTED}",
		"OUTPUT_FALLB": "${output:MISSING|dflt}",
		"ENV":          "${env:CALLER_VALUE}",
		"ENV_FALLB":    "${env:MISSING_VALUE|fallback}",
		"ENV_EMPTY":    "${env:MISSING_VALUE}",
		"SECRET":       "${secret:github-token}",
		"MIXED":        "${url:ui}/${database}",
	}, vars)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		// The azd environment wins over the shell for an output.
		"OUTPUT": "kv-plot",
		// An output the deployment did not produce is read from the shell,
		// which is how a repository with no azd environment supplies it.
		"OUTPUT_ENV":   "exported",
		"OUTPUT_FALLB": "dflt",
		"ENV":          "from-shell",
		"ENV_FALLB":    "fallback",
		"ENV_EMPTY":    "",
		"SECRET":       "secret:github-token",
		"MIXED":        "https://ca-plot-ui---comet.example.io/plot_pr_7",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExpandFull = %v, want %v", got, want)
	}
}

func TestExpandFullMissingOutputSaysWhatToDo(t *testing.T) {
	_, err := ExpandFull(map[string]string{"X": "${output:NOT_SET_ANYWHERE}"}, plotVars())
	if err == nil || !strings.Contains(err.Error(), "export it") {
		t.Errorf("err = %v, want it to say to set or export the value", err)
	}
}

func TestExpandFullWithoutAVault(t *testing.T) {
	_, err := ExpandFull(map[string]string{"X": "${secret:x}"}, plotVars())
	if err == nil {
		t.Error("a secret was resolved with no vault")
	}
}

func TestValidateLabel(t *testing.T) {
	for _, ok := range []string{"comet", "pr-42", "a1", "x"} {
		if err := ValidateLabel(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Comet", "a--b", "-a", "a-", strings.Repeat("a", 65)} {
		if err := ValidateLabel(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// ── Schema ───────────────────────────────────────────────────────────────

// Both consumers validate against the committed schema — the sales file as it
// is today, and the Plot file as it will be. The schema is generated, so this
// also catches a regeneration that was forgotten.
func TestFixturesValidateAgainstTheSchema(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	schema, err := compiler.Compile(filepath.Join("..", "..", "preview.schema.json"))
	if err != nil {
		t.Fatalf("compiling preview.schema.json: %v", err)
	}

	for _, fixture := range []string{"sales.preview.yaml", "plot.preview.yaml"} {
		contents, err := os.ReadFile(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatal(err)
		}
		var document any
		if err := yaml.Unmarshal(contents, &document); err != nil {
			t.Fatal(err)
		}
		// Through JSON, so the value shapes are the ones a validator expects.
		encoded, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		var generic any
		if err := json.Unmarshal(encoded, &generic); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(generic); err != nil {
			t.Errorf("%s does not validate: %v", fixture, err)
		}
	}

	// And the schema is strict: a mistyped key fails.
	var bad any
	_ = json.Unmarshal([]byte(`{"previewEnvironmnt":"prod"}`), &bad)
	if err := schema.Validate(bad); err == nil {
		t.Error("a mistyped key validated — additionalProperties must be false")
	}
	// previewEnvironment is no longer required.
	var minimal any
	_ = json.Unmarshal([]byte(`{"previewApp":"ca-x"}`), &minimal)
	if err := schema.Validate(minimal); err != nil {
		t.Errorf("a file without previewEnvironment fails the schema: %v", err)
	}
}
