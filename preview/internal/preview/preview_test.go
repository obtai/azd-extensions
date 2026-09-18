package preview

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"

	"github.com/obtai/azd-extensions/preview/internal/config"
)

func salesTarget() *Target {
	return &Target{
		Config:  &config.Config{LiveLabel: "live"},
		Project: "sales",
		Services: []Service{{
			Name:       "sales",
			ServiceApp: "ca-sales-prod",
			PreviewApp: "ca-sales-preview",
			Config:     config.Service{Image: "sales", HealthPath: "/api/health"},
		}},
		Domain: "happyhill-70162bb9.uksouth.azurecontainerapps.io",
	}
}

func plotTarget() *Target {
	return &Target{
		Config: &config.Config{
			LiveLabel: "live",
			Labels:    []string{"comet", "ember", "falcon"},
		},
		Project: "plot",
		Services: []Service{
			{Name: "api", ServiceApp: "ca-plot-api", PreviewApp: "ca-plot-api",
				Config: config.Service{Image: "api", HealthPath: "/api/health"}},
			{Name: "ui", ServiceApp: "ca-plot-ui", PreviewApp: "ca-plot-ui",
				Config: config.Service{Image: "ui", HealthPath: "/healthz"}},
		},
		Domain: "example.io",
	}
}

func TestNames(t *testing.T) {
	got := salesTarget().Names(42, DefaultLabel(42))

	if len(got.Services) != 1 {
		t.Fatalf("got %d services, want 1", len(got.Services))
	}
	service := got.Services[0]

	// Three dashes. Two would address a revision suffix, which changes every
	// push — the whole point of the label is that this URL does not move.
	want := "https://ca-sales-preview---pr-42.happyhill-70162bb9.uksouth.azurecontainerapps.io"
	if service.URL != want {
		t.Errorf("URL = %q, want %q", service.URL, want)
	}
	if service.FQDN != strings.TrimPrefix(want, "https://") {
		t.Errorf("FQDN = %q, want the URL without its scheme", service.FQDN)
	}
	// The PREVIEW app, not the one azd deploys.
	if service.App != "ca-sales-preview" {
		t.Errorf("App = %q, want the preview app", service.App)
	}
	// The shorthand pushes to the project's repository, as 0.5.0 did.
	if service.Repository != "sales" {
		t.Errorf("Repository = %q, want sales", service.Repository)
	}
	if got.Label != "pr-42" {
		t.Errorf("Label = %q, want pr-42", got.Label)
	}
	if got.Database != "sales_pr_42" {
		t.Errorf("Database = %q, want sales_pr_42", got.Database)
	}
	if got.TagPrefix != "pr-42-" {
		t.Errorf("TagPrefix = %q, want pr-42-", got.TagPrefix)
	}
	// No dash between "pr" and the number: a revision suffix may not contain
	// two consecutive dashes once the stamp is appended to a prefix.
	if got.SuffixPrefix != "pr42-" {
		t.Errorf("SuffixPrefix = %q, want pr42-", got.SuffixPrefix)
	}
}

func TestNamesPerService(t *testing.T) {
	got := plotTarget().Names(7, "comet")

	urls := got.urls()
	if urls["api"] != "https://ca-plot-api---comet.example.io" ||
		urls["ui"] != "https://ca-plot-ui---comet.example.io" {
		t.Errorf("urls = %v", urls)
	}
	// Each service pushes to its own repository.
	if got.Services[0].Repository != "api" || got.Services[1].Repository != "ui" {
		t.Errorf("repositories = %q, %q", got.Services[0].Repository, got.Services[1].Repository)
	}
	if got.Services[1].HealthPath != "/healthz" {
		t.Errorf("ui health path = %q", got.Services[1].HealthPath)
	}
}

func TestNamesDatabaseOverride(t *testing.T) {
	target := plotTarget()
	target.Config.Database = &config.Database{Name: "plot_${label}_${pr}"}
	if got := target.Names(7, "comet").Database; got != "plot_comet_7" {
		t.Errorf("Database = %q, want plot_comet_7", got)
	}
}

func TestNamesLabelIsStableAcrossPushes(t *testing.T) {
	target := salesTarget()
	if target.Names(7, "pr-7").Services[0].URL != target.Names(7, "pr-7").Services[0].URL {
		t.Fatal("the same pull request produced two URLs")
	}
	if target.Names(7, "pr-7").Services[0].URL == target.Names(8, "pr-8").Services[0].URL {
		t.Fatal("two pull requests share a URL")
	}
}

func env(pairs ...string) []*armappcontainers.EnvironmentVar {
	vars := make([]*armappcontainers.EnvironmentVar, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		vars = append(vars, &armappcontainers.EnvironmentVar{
			Name:  to.Ptr(pairs[i]),
			Value: to.Ptr(pairs[i+1]),
		})
	}
	return vars
}

func envMap(vars []*armappcontainers.EnvironmentVar) map[string]string {
	found := map[string]string{}
	for _, entry := range vars {
		if entry == nil || entry.Name == nil {
			continue
		}
		value := ""
		if entry.Value != nil {
			value = *entry.Value
		}
		found[*entry.Name] = value
	}
	return found
}

func productionTemplate() *armappcontainers.Template {
	return &armappcontainers.Template{
		RevisionSuffix: to.Ptr("abc1234"),
		Containers: []*armappcontainers.Container{{
			Name:  to.Ptr("sales"),
			Image: to.Ptr("acr.io/sales:latest"),
			Env: env(
				"APP_ENV", "production",
				"PG_DATABASE", "sales",
				"PG_POOL_MAX", "10",
				"AI_MODEL_DEPLOYMENT", "gpt-5.4",
			),
		}},
		Scale: &armappcontainers.Scale{MinReplicas: to.Ptr(int32(1)), MaxReplicas: to.Ptr(int32(1))},
	}
}

func TestPreviewTemplateOverridesAndInherits(t *testing.T) {
	live := productionTemplate()

	got, err := previewTemplate(live, "acr.io/sales:pr-42-abc1234", "", map[string]string{
		"APP_ENV":     "preview",
		"PG_DATABASE": "sales_pr_42",
		"APP_URL":     "https://ca-sales-prod---pr-42.example.io",
	}, "pr42-abc1234", 0, 1)
	if err != nil {
		t.Fatal(err)
	}

	values := envMap(got.Containers[0].Env)

	// Overridden.
	if values["APP_ENV"] != "preview" {
		t.Errorf("APP_ENV = %q, want preview", values["APP_ENV"])
	}
	if values["PG_DATABASE"] != "sales_pr_42" {
		t.Errorf("PG_DATABASE = %q, want sales_pr_42", values["PG_DATABASE"])
	}
	// Appended, because production has no such variable.
	if values["APP_URL"] != "https://ca-sales-prod---pr-42.example.io" {
		t.Errorf("APP_URL = %q, want the label URL", values["APP_URL"])
	}
	// Inherited. This is the property that lets a new variable in the
	// infrastructure reach previews without preview.yaml being touched.
	if values["AI_MODEL_DEPLOYMENT"] != "gpt-5.4" {
		t.Errorf("AI_MODEL_DEPLOYMENT = %q, want it inherited", values["AI_MODEL_DEPLOYMENT"])
	}
	if values["PG_POOL_MAX"] != "10" {
		t.Errorf("PG_POOL_MAX = %q, want it inherited", values["PG_POOL_MAX"])
	}

	if got.RevisionSuffix == nil || *got.RevisionSuffix != "pr42-abc1234" {
		t.Errorf("RevisionSuffix = %v, want pr42-abc1234", got.RevisionSuffix)
	}
	if *got.Containers[0].Image != "acr.io/sales:pr-42-abc1234" {
		t.Errorf("Image = %q", *got.Containers[0].Image)
	}
	// An idle preview must cost nothing.
	if *got.Scale.MinReplicas != 0 {
		t.Errorf("MinReplicas = %d, want 0", *got.Scale.MinReplicas)
	}
}

// The hazard, at the unit level: deriving a preview must not touch the template
// it was derived from, or the restore would put the preview's own environment
// back onto the app.
func TestPreviewTemplateDoesNotMutateLive(t *testing.T) {
	live := productionTemplate()

	if _, err := previewTemplate(live, "acr.io/sales:pr-42-abc1234", "", map[string]string{
		"APP_ENV":     "preview",
		"PG_DATABASE": "sales_pr_42",
	}, "pr42-abc1234", 0, 1); err != nil {
		t.Fatal(err)
	}

	values := envMap(live.Containers[0].Env)
	if values["APP_ENV"] != "production" {
		t.Errorf("live APP_ENV = %q, want production — the live template was mutated", values["APP_ENV"])
	}
	if values["PG_DATABASE"] != "sales" {
		t.Errorf("live PG_DATABASE = %q, want sales — the live template was mutated", values["PG_DATABASE"])
	}
	if *live.Containers[0].Image != "acr.io/sales:latest" {
		t.Errorf("live image = %q, want it untouched", *live.Containers[0].Image)
	}
	if *live.Scale.MinReplicas != 1 {
		t.Errorf("live MinReplicas = %d, want 1", *live.Scale.MinReplicas)
	}
}

func sidecarTemplate() *armappcontainers.Template {
	return &armappcontainers.Template{
		RevisionSuffix: to.Ptr("abc1234"),
		InitContainers: []*armappcontainers.InitContainer{{
			Name: to.Ptr("migrate"), Image: to.Ptr("acr.io/api:latest"),
		}},
		Containers: []*armappcontainers.Container{
			{
				Name:  to.Ptr("otel"),
				Image: to.Ptr("otel/collector:1"),
				Env:   env("OTEL_MODE", "agent"),
			},
			{
				Name:  to.Ptr("api"),
				Image: to.Ptr("acr.io/api:latest"),
				Env:   env("DB_NAME", "plot"),
				VolumeMounts: []*armappcontainers.VolumeMount{{
					VolumeName: to.Ptr("scratch"), MountPath: to.Ptr("/scratch"),
				}},
			},
		},
		Volumes: []*armappcontainers.Volume{{
			Name: to.Ptr("scratch"), StorageType: to.Ptr(armappcontainers.StorageTypeEmptyDir),
		}},
		ServiceBinds:                  []*armappcontainers.ServiceBind{{Name: to.Ptr("redis")}},
		TerminationGracePeriodSeconds: to.Ptr(int64(45)),
		Scale: &armappcontainers.Scale{
			MinReplicas: to.Ptr(int32(2)), MaxReplicas: to.Ptr(int32(5)),
			Rules: []*armappcontainers.ScaleRule{{Name: to.Ptr("http")}},
		},
	}
}

// A template is more than its first container. An app with a sidecar, an init
// container or a mounted share is not the same app without them.
func TestPreviewTemplateKeepsTheWholeTemplate(t *testing.T) {
	live := sidecarTemplate()

	got, err := previewTemplate(live, "acr.io/api:pr-7-abc1234", "api",
		map[string]string{"DB_NAME": "plot_pr_7"}, "pr7-abc1234", 0, 2)
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Containers) != 2 {
		t.Fatalf("got %d containers, want both", len(got.Containers))
	}
	// The named container, not the first.
	if *got.Containers[1].Image != "acr.io/api:pr-7-abc1234" {
		t.Errorf("api image = %q, want the preview's", *got.Containers[1].Image)
	}
	if envMap(got.Containers[1].Env)["DB_NAME"] != "plot_pr_7" {
		t.Errorf("api env = %v, want DB_NAME overridden", envMap(got.Containers[1].Env))
	}
	if len(got.Containers[1].VolumeMounts) != 1 {
		t.Error("the api container lost its volume mount")
	}
	// The sidecar is untouched.
	if *got.Containers[0].Image != "otel/collector:1" {
		t.Errorf("sidecar image = %q, want it untouched", *got.Containers[0].Image)
	}
	if envMap(got.Containers[0].Env)["OTEL_MODE"] != "agent" {
		t.Error("the sidecar's env was changed")
	}
	if len(got.InitContainers) != 1 || len(got.Volumes) != 1 || len(got.ServiceBinds) != 1 {
		t.Errorf("init containers %d, volumes %d, binds %d — want 1 each",
			len(got.InitContainers), len(got.Volumes), len(got.ServiceBinds))
	}
	if got.TerminationGracePeriodSeconds == nil || *got.TerminationGracePeriodSeconds != 45 {
		t.Error("the grace period was dropped")
	}
	// Scale is the preview's own: the configured range, and no production rules.
	if *got.Scale.MinReplicas != 0 || *got.Scale.MaxReplicas != 2 {
		t.Errorf("scale = %d–%d, want 0–2", *got.Scale.MinReplicas, *got.Scale.MaxReplicas)
	}
	if len(got.Scale.Rules) != 0 {
		t.Error("production's scale rules came along")
	}
	// And the live template is as it was.
	if *live.Containers[1].Image != "acr.io/api:latest" || *live.Scale.MinReplicas != 2 {
		t.Error("the live template was mutated")
	}
}

func TestPreviewTemplateRefusesAnUnknownContainer(t *testing.T) {
	if _, err := previewTemplate(sidecarTemplate(), "x", "worker", nil, "s", 0, 1); err == nil {
		t.Error("an unknown container name was accepted — the image would have gone nowhere")
	}
}

func TestScaleOf(t *testing.T) {
	if lo, hi := scaleOf(nil); lo != 0 || hi != 1 {
		t.Errorf("default scale = %d–%d, want 0–1", lo, hi)
	}
	if lo, hi := scaleOf(&config.Scale{Max: to.Ptr(int32(3))}); lo != 0 || hi != 3 {
		t.Errorf("scale = %d–%d, want 0–3", lo, hi)
	}
}

func traffic(entries ...*armappcontainers.TrafficWeight) []*armappcontainers.TrafficWeight {
	return entries
}

func weight(revision, label string, w int32) *armappcontainers.TrafficWeight {
	entry := &armappcontainers.TrafficWeight{Weight: to.Ptr(w)}
	if revision != "" {
		entry.RevisionName = to.Ptr(revision)
	}
	if label != "" {
		entry.Label = to.Ptr(label)
	}
	return entry
}

func total(entries []*armappcontainers.TrafficWeight) int32 {
	var sum int32
	for _, entry := range entries {
		if entry != nil && entry.Weight != nil {
			sum += *entry.Weight
		}
	}
	return sum
}

func TestWithLabelLeavesTheSplitAlone(t *testing.T) {
	existing := traffic(
		weight("ca-sales-prod--abc1234", "live", 100),
		weight("ca-sales-prod--pr51-def5678", "pr-51", 0),
	)

	got := withLabel(existing, "pr-42", "ca-sales-prod--pr42-abc1234")

	if total(got) != 100 {
		t.Errorf("weights total %d, want 100 — Container Apps rejects anything else", total(got))
	}
	if !hasLabel(got, "live") || !hasLabel(got, "pr-51") || !hasLabel(got, "pr-42") {
		t.Errorf("labels = %v, want live, pr-51 and pr-42", got)
	}
	for _, entry := range got {
		if *entry.Label == "live" && *entry.Weight != 100 {
			t.Errorf("live weight = %d, want 100", *entry.Weight)
		}
		if *entry.Label == "pr-42" && *entry.Weight != 0 {
			t.Errorf("pr-42 weight = %d, want 0 — a preview never takes traffic", *entry.Weight)
		}
	}
}

// A second push must move the label rather than add a second entry for it.
func TestWithLabelReplacesRatherThanDuplicates(t *testing.T) {
	existing := traffic(
		weight("ca-sales-prod--abc1234", "live", 100),
		weight("ca-sales-prod--pr42-old", "pr-42", 0),
	)

	got := withLabel(existing, "pr-42", "ca-sales-prod--pr42-new")

	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if total(got) != 100 {
		t.Errorf("weights total %d, want 100", total(got))
	}
	for _, entry := range got {
		if *entry.Label == "pr-42" && *entry.RevisionName != "ca-sales-prod--pr42-new" {
			t.Errorf("pr-42 points at %q, want the new revision", *entry.RevisionName)
		}
	}
}

func TestWithoutLabel(t *testing.T) {
	existing := traffic(
		weight("ca-sales-prod--abc1234", "live", 100),
		weight("ca-sales-prod--pr42-abc", "pr-42", 0),
	)

	got := withoutLabel(existing, "pr-42")

	if hasLabel(got, "pr-42") {
		t.Error("pr-42 survived teardown")
	}
	if !hasLabel(got, "live") {
		t.Error("teardown removed the live label")
	}
	if total(got) != 100 {
		t.Errorf("weights total %d, want 100", total(got))
	}
}

// Bicep declares the live entry as a label with no revision name until the
// first release assigns it. Teardown must survive that state.
func TestWithoutLabelToleratesAnUnassignedLiveEntry(t *testing.T) {
	existing := traffic(weight("", "live", 100), weight("ca-sales-prod--pr42-abc", "pr-42", 0))

	got := withoutLabel(existing, "pr-42")

	if len(got) != 1 || total(got) != 100 {
		t.Errorf("got %d entries totalling %d, want 1 totalling 100", len(got), total(got))
	}
}

func appWith(name string, mode armappcontainers.ActiveRevisionsMode, entries ...*armappcontainers.TrafficWeight) *armappcontainers.ContainerApp {
	return &armappcontainers.ContainerApp{
		Name: to.Ptr(name),
		Properties: &armappcontainers.ContainerAppProperties{
			Configuration: &armappcontainers.Configuration{
				ActiveRevisionsMode: to.Ptr(mode),
				Ingress:             &armappcontainers.Ingress{Traffic: entries},
			},
		},
	}
}

func TestMultipleRevisionModeRefusesSingle(t *testing.T) {
	if err := multipleRevisionMode(appWith("ca-sales-prod", armappcontainers.ActiveRevisionsModeSingle)); err == nil {
		t.Error("single-revision mode was accepted — a preview would have taken production's traffic")
	}
	if err := multipleRevisionMode(appWith("ca-sales-prod", armappcontainers.ActiveRevisionsModeMultiple)); err != nil {
		t.Errorf("multiple-revision mode was refused: %v", err)
	}
}

// The restore must not carry the live revision's suffix back onto the app.
// Container Apps rejects a PUT naming a suffix that already exists — it does
// not treat it as the no-op it looks like — and a failed restore leaves the
// preview's database name on the app that serves production.
func TestForRestoreClearsTheSuffix(t *testing.T) {
	live := productionTemplate()
	if live.RevisionSuffix == nil {
		t.Fatal("fixture should carry a suffix")
	}

	restored := forRestore(live)

	if restored.RevisionSuffix != nil {
		t.Errorf("RevisionSuffix = %q, want nil — this is the PUT Azure rejects",
			*restored.RevisionSuffix)
	}
	if imageOf(restored, "") != imageOf(live, "") {
		t.Error("the restore changed the image it was meant to put back")
	}
	// The caller still needs the original, so clearing must not reach through.
	if live.RevisionSuffix == nil {
		t.Error("forRestore mutated the template it was given")
	}
}

func TestForRestoreToleratesNil(t *testing.T) {
	if forRestore(nil) != nil {
		t.Error("forRestore(nil) should be nil")
	}
}

// Which shape a service is in decides whether the app template has to be put
// back after a preview is written to it — the single most consequential branch
// in this package.
func TestDedicated(t *testing.T) {
	shared := Service{ServiceApp: "ca-sales-prod", PreviewApp: "ca-sales-prod"}
	if shared.dedicated() {
		t.Error("previews on the service app are not dedicated — the restore must run")
	}

	own := Service{ServiceApp: "ca-sales-prod", PreviewApp: "ca-sales-preview"}
	if !own.dedicated() {
		t.Error("previews on their own app are dedicated — there is nothing to restore")
	}
}

// ── Label pool ───────────────────────────────────────────────────────────

var pool = []string{"comet", "ember", "falcon"}

func TestChooseLabelKeepsTheOneHeld(t *testing.T) {
	apps := []appTraffic{
		{App: "ca-plot-api", Traffic: traffic(
			weight("ca-plot-api--abc", "live", 100),
			weight("ca-plot-api--pr3-aaa", "comet", 0),
			weight("ca-plot-api--pr7-bbb", "ember", 0),
		)},
		{App: "ca-plot-ui", Traffic: traffic(
			weight("ca-plot-ui--abc", "live", 100),
			weight("ca-plot-ui--pr3-aaa", "comet", 0),
			weight("ca-plot-ui--pr7-bbb", "ember", 0),
		)},
	}

	// Iterating on a pull request never moves its link.
	got, err := chooseLabel(pool, "pr7-", apps)
	if err != nil || got != "ember" {
		t.Errorf("chooseLabel = %q, %v — want ember, the label PR 7 already holds", got, err)
	}
}

func TestChooseLabelTakesTheFirstFree(t *testing.T) {
	apps := []appTraffic{
		{App: "ca-plot-api", Traffic: traffic(
			weight("ca-plot-api--abc", "live", 100),
			weight("ca-plot-api--pr3-aaa", "comet", 0),
		)},
		// The ui app holds ember for somebody, so it is not free anywhere.
		{App: "ca-plot-ui", Traffic: traffic(
			weight("ca-plot-ui--abc", "live", 100),
			weight("ca-plot-ui--pr3-aaa", "comet", 0),
			weight("ca-plot-ui--pr5-ccc", "ember", 0),
		)},
	}

	got, err := chooseLabel(pool, "pr7-", apps)
	if err != nil || got != "falcon" {
		t.Errorf("chooseLabel = %q, %v — want falcon, the first label free on every app", got, err)
	}
}

// A label whose revision belongs to another pull request is not this one's,
// even when the prefix looks alike: pr7- is not pr70-.
func TestChooseLabelDoesNotConfusePrefixes(t *testing.T) {
	apps := []appTraffic{{App: "ca-plot-api", Traffic: traffic(
		weight("ca-plot-api--abc", "live", 100),
		weight("ca-plot-api--pr70-aaa", "comet", 0),
	)}}

	got, err := chooseLabel(pool, "pr7-", apps)
	if err != nil || got != "ember" {
		t.Errorf("chooseLabel = %q, %v — want ember; comet belongs to PR 70", got, err)
	}
}

func TestChooseLabelFailsWhenExhausted(t *testing.T) {
	apps := []appTraffic{{App: "ca-plot-api", Traffic: traffic(
		weight("ca-plot-api--abc", "live", 100),
		weight("ca-plot-api--pr1-a", "comet", 0),
		weight("ca-plot-api--pr2-b", "ember", 0),
		weight("ca-plot-api--pr3-c", "falcon", 0),
	)}}

	_, err := chooseLabel(pool, "pr7-", apps)
	if err == nil || !strings.Contains(err.Error(), "all 3 preview labels are in use") {
		t.Errorf("err = %v, want the pool reported full", err)
	}
}

func TestHeldLabelIsEmptyWithoutAPreview(t *testing.T) {
	apps := []appTraffic{{App: "ca-plot-api", Traffic: traffic(weight("ca-plot-api--abc", "live", 100))}}
	if got := heldLabel(pool, "pr7-", apps); got != "" {
		t.Errorf("heldLabel = %q, want nothing", got)
	}
}

// ── Base template on a shared app ────────────────────────────────────────

func TestBaseRevisionPrefersTheLiveLabel(t *testing.T) {
	app := appWith("ca-sales-prod", armappcontainers.ActiveRevisionsModeMultiple,
		weight("ca-sales-prod--v2", "live", 100),
		weight("ca-sales-prod--pr42-x", "pr-42", 0),
	)
	app.Properties.LatestRevisionName = to.Ptr("ca-sales-prod--pr42-x")

	got, err := baseRevision(app, "live")
	if err != nil || got != "ca-sales-prod--v2" {
		t.Errorf("baseRevision = %q, %v — want the labelled revision", got, err)
	}
}

// No label: the revision holding all the traffic, by name.
func TestBaseRevisionFallsBackToFullTraffic(t *testing.T) {
	app := appWith("ca-plot-api", armappcontainers.ActiveRevisionsModeMultiple,
		weight("ca-plot-api--v2", "", 100),
		weight("ca-plot-api--pr42-x", "comet", 0),
	)
	// The latest revision is the PREVIEW, which is exactly what the old
	// fallback would have derived the next one from.
	app.Properties.LatestRevisionName = to.Ptr("ca-plot-api--pr42-x")

	got, err := baseRevision(app, "live")
	if err != nil || got != "ca-plot-api--v2" {
		t.Errorf("baseRevision = %q, %v — want the revision holding 100%%", got, err)
	}
}

// A fresh `azd deploy` leaves `latestRevision: true` at 100 with no name; the
// app says which revision that is.
func TestBaseRevisionResolvesLatestRevisionEntry(t *testing.T) {
	entry := &armappcontainers.TrafficWeight{LatestRevision: to.Ptr(true), Weight: to.Ptr(int32(100))}
	app := appWith("ca-plot-api", armappcontainers.ActiveRevisionsModeMultiple, entry)
	app.Properties.LatestRevisionName = to.Ptr("ca-plot-api--v3")

	got, err := baseRevision(app, "live")
	if err != nil || got != "ca-plot-api--v3" {
		t.Errorf("baseRevision = %q, %v — want the app's latest revision", got, err)
	}
}

// A split is nobody's fault but nobody's answer either.
func TestBaseRevisionRefusesASplit(t *testing.T) {
	app := appWith("ca-plot-api", armappcontainers.ActiveRevisionsModeMultiple,
		weight("ca-plot-api--v2", "", 50),
		weight("ca-plot-api--v3", "", 50),
	)
	app.Properties.LatestRevisionName = to.Ptr("ca-plot-api--v3")

	if _, err := baseRevision(app, "live"); err == nil {
		t.Error("a 50/50 split produced a base revision")
	}
}

// ── Revision garbage collection ──────────────────────────────────────────

func TestStaleRevisionsKeepsTheNewOne(t *testing.T) {
	active := []string{
		"ca-plot-api--v2",
		"ca-plot-api--pr7-aaa",
		"ca-plot-api--pr7-bbb",
		"ca-plot-api--pr70-ccc",
		"ca-plot-api--pr7-new",
	}

	got := staleRevisions(active, "ca-plot-api--pr7-", "ca-plot-api--pr7-new")

	want := []string{"ca-plot-api--pr7-aaa", "ca-plot-api--pr7-bbb"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("staleRevisions = %v, want %v — not production, not PR 70, not the new one", got, want)
	}
}

func TestStaleTagsKeepsThisPush(t *testing.T) {
	got := staleTags([]string{"pr-7-aaa", "pr-7-bbb", "pr-7-new"}, "pr-7-new")
	if !reflect.DeepEqual(got, []string{"pr-7-aaa", "pr-7-bbb"}) {
		t.Errorf("staleTags = %v", got)
	}
}

// ── Promote ──────────────────────────────────────────────────────────────

func TestPromotedMovesAllTrafficAndKeepsLabels(t *testing.T) {
	existing := traffic(
		weight("ca-plot-api--v2", "live", 100),
		weight("ca-plot-api--pr7-x", "comet", 0),
	)

	got, revision, err := promoted(existing, "comet")
	if err != nil {
		t.Fatal(err)
	}
	if revision != "ca-plot-api--pr7-x" {
		t.Errorf("revision = %q", revision)
	}
	if total(got) != 100 || len(got) != 2 {
		t.Errorf("got %d entries totalling %d", len(got), total(got))
	}
	for _, entry := range got {
		switch *entry.Label {
		case "comet":
			if *entry.Weight != 100 {
				t.Errorf("comet weight = %d, want 100", *entry.Weight)
			}
		case "live":
			if *entry.Weight != 0 {
				t.Errorf("live weight = %d, want 0 — the label stays, the traffic moves", *entry.Weight)
			}
		}
	}
	// The input is not touched.
	if *existing[0].Weight != 100 {
		t.Error("promoted mutated its input")
	}
}

func TestPromotedRefusesAMissingLabel(t *testing.T) {
	if _, _, err := promoted(traffic(weight("ca-plot-api--v2", "live", 100)), "comet"); err == nil {
		t.Error("a label nobody carries was promoted")
	}
}
