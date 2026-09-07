package preview

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
)

func TestNames(t *testing.T) {
	target := &Target{
		Project:   "sales",
		SourceApp: "ca-sales-prod",
		Domain:    "happyhill-70162bb9.uksouth.azurecontainerapps.io",
	}

	got := target.Names(42)

	// Three dashes. Two would address a revision suffix, which changes every
	// push — the whole point of the label is that this URL does not move.
	want := "https://ca-sales-prod---pr-42.happyhill-70162bb9.uksouth.azurecontainerapps.io"
	if got.URL != want {
		t.Errorf("URL = %q, want %q", got.URL, want)
	}
	if got.App != "ca-sales-prod" {
		t.Errorf("App = %q, want the source app", got.App)
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

func TestNamesLabelIsStableAcrossPushes(t *testing.T) {
	target := &Target{Project: "sales", SourceApp: "ca-sales-prod", Domain: "example.io"}
	if target.Names(7).URL != target.Names(7).URL {
		t.Fatal("the same pull request produced two URLs")
	}
	if target.Names(7).URL == target.Names(8).URL {
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

	got := previewTemplate(live, "acr.io/sales:pr-42-abc1234", map[string]string{
		"APP_ENV":     "preview",
		"PG_DATABASE": "sales_pr_42",
		"APP_URL":     "https://ca-sales-prod---pr-42.example.io",
	}, "pr42-abc1234")

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

	previewTemplate(live, "acr.io/sales:pr-42-abc1234", map[string]string{
		"APP_ENV":     "preview",
		"PG_DATABASE": "sales_pr_42",
	}, "pr42-abc1234")

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

func TestMultipleRevisionModeRefusesSingle(t *testing.T) {
	app := func(mode armappcontainers.ActiveRevisionsMode) *armappcontainers.ContainerApp {
		return &armappcontainers.ContainerApp{
			Name: to.Ptr("ca-sales-prod"),
			Properties: &armappcontainers.ContainerAppProperties{
				Configuration: &armappcontainers.Configuration{
					ActiveRevisionsMode: to.Ptr(mode),
					Ingress:             &armappcontainers.Ingress{},
				},
			},
		}
	}

	if err := multipleRevisionMode(app(armappcontainers.ActiveRevisionsModeSingle)); err == nil {
		t.Error("single-revision mode was accepted — a preview would have taken production's traffic")
	}
	if err := multipleRevisionMode(app(armappcontainers.ActiveRevisionsModeMultiple)); err != nil {
		t.Errorf("multiple-revision mode was refused: %v", err)
	}
}
