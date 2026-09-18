package cmd

import (
	"strings"
	"testing"

	"github.com/obtai/azd-extensions/preview/internal/config"
)

func TestEnvironmentPrefix(t *testing.T) {
	for project, want := range map[string]string{
		"sales": "SALES", "my-app": "MY_APP", "obt.plot": "OBT_PLOT", "api": "API",
	} {
		if got := environmentPrefix(project); got != want {
			t.Errorf("environmentPrefix(%q) = %q, want %q", project, got, want)
		}
	}
}

// The one order that matters: what preview.yaml says, then the azd
// environment, then the shell.
func TestResolverOrder(t *testing.T) {
	t.Setenv("AZURE_RESOURCE_GROUP", "rg-from-shell")
	t.Setenv("APP_FROM_SHELL", "shell-app")

	resolve := resolver{
		values:      map[string]string{"AZURE_RESOURCE_GROUP": "rg-from-azd", "APP_X": "x-from-azd"},
		environment: "dev",
		vars:        config.Vars{Outputs: map[string]string{"AZURE_RESOURCE_GROUP": "rg-from-azd", "APP_X": "x-from-azd"}},
	}

	// Explicit wins, and is itself a substitution.
	if got, _ := resolve.required("${output:APP_X}", "outputs.resourceGroup", "AZURE_RESOURCE_GROUP"); got != "x-from-azd" {
		t.Errorf("explicit = %q, want the substituted value", got)
	}
	// An explicit ${output:} the environment lacks reads the shell.
	if got, _ := resolve.required("${output:APP_FROM_SHELL}", "outputs.x", ""); got != "shell-app" {
		t.Errorf("explicit from shell = %q", got)
	}
	// Then the azd environment, over the shell.
	if got, _ := resolve.required("", "", "AZURE_RESOURCE_GROUP"); got != "rg-from-azd" {
		t.Errorf("conventional = %q, want the azd value", got)
	}
	// Then the shell.
	if got, _ := resolve.required("", "", "APP_FROM_SHELL"); got != "shell-app" {
		t.Errorf("shell fallback = %q", got)
	}
	// Then an error that says what to do.
	_, err := resolve.required("", "", "APP_NOWHERE")
	if err == nil || !strings.Contains(err.Error(), "azd env refresh -e dev") || !strings.Contains(err.Error(), "export it") {
		t.Errorf("err = %v", err)
	}
	// Optional values are simply empty.
	if got, err := resolve.optional("", "", "APP_NOWHERE"); got != "" || err != nil {
		t.Errorf("optional = %q, %v", got, err)
	}
}

// Without an azd environment there is nothing to refresh, and the message
// must not suggest it.
func TestResolverWithoutAnEnvironment(t *testing.T) {
	resolve := resolver{values: map[string]string{}, vars: config.Vars{Outputs: map[string]string{}}}
	_, err := resolve.required("", "", "AZURE_SUBSCRIPTION_ID")
	if err == nil || strings.Contains(err.Error(), "azd env refresh") ||
		!strings.Contains(err.Error(), "export it") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveServicesShorthand(t *testing.T) {
	settings, err := config.Parse([]byte("previewApp: ${output:SALES_PREVIEW_APP_NAME}\n"))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"SALES_CONTAINER_APP_NAME": "ca-sales-prod",
		"SALES_PREVIEW_APP_NAME":   "ca-sales-preview",
	}
	resolve := resolver{values: values, environment: "prod", vars: config.Vars{Outputs: values}}

	services, err := resolveServices(settings, resolve, "sales", "SALES")
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 {
		t.Fatalf("got %d services", len(services))
	}
	service := services[0]
	if service.Name != "sales" || service.ServiceApp != "ca-sales-prod" || service.PreviewApp != "ca-sales-preview" {
		t.Errorf("service = %+v", service)
	}
	if service.Config.Image != "sales" {
		t.Errorf("Image = %q, want the project name", service.Config.Image)
	}
}

// Under `services`, `app` is the app azd deploys — previews are revisions of
// it — and it defaults to <PREFIX>_<SERVICE>_CONTAINER_APP_NAME.
func TestResolveServicesNamed(t *testing.T) {
	t.Setenv("APP_UI_CONTAINER_APP_NAME", "ca-plot-ui")
	settings, err := config.Parse([]byte(
		"prefix: APP\nservices:\n  api:\n    app: ${output:APP_API_CONTAINER_APP_NAME}\n  ui: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"APP_API_CONTAINER_APP_NAME": "ca-plot-api"}
	resolve := resolver{values: values, environment: "dev", vars: config.Vars{Outputs: values}}

	services, err := resolveServices(settings, resolve, "plot", "APP")
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 2 {
		t.Fatalf("got %d services", len(services))
	}
	if services[0].PreviewApp != "ca-plot-api" || services[0].ServiceApp != "ca-plot-api" {
		t.Errorf("api = %+v", services[0])
	}
	// ui had no `app`: the conventional name, read from the shell.
	if services[1].PreviewApp != "ca-plot-ui" || services[1].ServiceApp != "ca-plot-ui" {
		t.Errorf("ui = %+v", services[1])
	}
	if services[0].Config.Image != "api" || services[1].Config.Image != "ui" {
		t.Errorf("images = %q, %q", services[0].Config.Image, services[1].Config.Image)
	}
}

func TestResolveServicesRefusesASharedApp(t *testing.T) {
	settings, err := config.Parse([]byte("services:\n  api:\n    app: ca-one\n  ui:\n    app: ca-one\n"))
	if err != nil {
		t.Fatal(err)
	}
	resolve := resolver{values: map[string]string{}, vars: config.Vars{Outputs: map[string]string{}}}
	if _, err := resolveServices(settings, resolve, "plot", "PLOT"); err == nil {
		t.Error("two services on one app were accepted — their labels would collide")
	}
}
