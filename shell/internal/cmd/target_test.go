package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"

	"github.com/obtai/azd-extensions/shell/internal/ui"
)

func app(group, name, service string) *armappcontainers.ContainerApp {
	a := &armappcontainers.ContainerApp{
		ID:   to.Ptr("/subscriptions/sub/resourceGroups/" + group + "/providers/Microsoft.App/containerApps/" + name),
		Name: to.Ptr(name),
		Tags: map[string]*string{},
	}
	if service != "" {
		a.Tags[serviceTag] = to.Ptr(service)
	}
	return a
}

func names(apps []*armappcontainers.ContainerApp) string {
	var out []string
	for _, a := range apps {
		out = append(out, *a.Name)
	}
	return strings.Join(out, ",")
}

func TestMatchApps(t *testing.T) {
	apps := []*armappcontainers.ContainerApp{
		app("rg-dev", "ca-api-abc", "api"),
		app("rg-dev", "ca-web-abc", "web"),
		app("rg-prod", "ca-api-xyz", "api"),
		app("rg-dev", "worker", ""),
	}
	tests := map[string]string{
		"api":        "ca-api-abc,ca-api-xyz", // by service, across groups
		"API":        "ca-api-abc,ca-api-xyz",
		"ca-web-abc": "ca-web-abc", // by app name
		"worker":     "worker",     // untagged
		"nope":       "",
	}
	for name, want := range tests {
		if got := names(matchApps(apps, name)); got != want {
			t.Errorf("matchApps(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestGroupsOf(t *testing.T) {
	apps := []*armappcontainers.ContainerApp{
		app("rg-prod", "a", ""), app("rg-dev", "b", ""), app("rg-prod", "c", ""),
	}
	if got := strings.Join(groupsOf(apps), ","); got != "rg-dev,rg-prod" {
		t.Errorf("groupsOf = %q", got)
	}
}

func TestAppLabel(t *testing.T) {
	if got := appLabel(app("rg-dev", "ca-api-abc", "api"), true); got != "api (ca-api-abc) · rg-dev" {
		t.Errorf("appLabel = %q", got)
	}
	if got := appLabel(app("rg-dev", "worker", ""), false); got != "worker" {
		t.Errorf("appLabel = %q", got)
	}
}

func TestSortRevisions(t *testing.T) {
	now := time.Now()
	revision := func(name string, weight int32, age time.Duration) *armappcontainers.Revision {
		return &armappcontainers.Revision{Name: to.Ptr(name), Properties: &armappcontainers.RevisionProperties{
			TrafficWeight: to.Ptr(weight), CreatedTime: to.Ptr(now.Add(-age)),
		}}
	}
	revisions := []*armappcontainers.Revision{
		revision("old-preview", 0, 48*time.Hour),
		revision("live", 100, 72*time.Hour),
		revision("new-preview", 0, time.Hour),
	}
	sortRevisions(revisions)
	var got []string
	for _, r := range revisions {
		got = append(got, *r.Name)
	}
	if strings.Join(got, ",") != "live,new-preview,old-preview" {
		t.Errorf("sortRevisions = %v", got)
	}
}

func TestPickWithoutPrompt(t *testing.T) {
	ctx := context.Background()
	quiet := ui.UI{Interactive: false}
	choice := ui.Choice{Title: "Container app", Noun: "container apps", Flag: "--app"}

	one := []ui.Item[string]{{Label: "api", Value: "ca-api"}}
	if got, err := ui.Pick(ctx, quiet, choice, one); err != nil || got != "ca-api" {
		t.Errorf("one item: got %q, %v", got, err)
	}

	_, err := ui.Pick(ctx, quiet, choice, append(one, ui.Item[string]{Label: "web", Value: "ca-web"}))
	if err == nil || !strings.Contains(err.Error(), "pass --app") || !strings.Contains(err.Error(), "web") {
		t.Errorf("two items without a prompt: err = %v", err)
	}

	if _, err := ui.Pick[string](ctx, quiet, choice, nil); err == nil || !strings.Contains(err.Error(), "no container apps") {
		t.Errorf("no items: err = %v", err)
	}
}
