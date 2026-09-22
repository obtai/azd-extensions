package cmd

import (
	"net/netip"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/postgresql/armpostgresqlflexibleservers/v4"
)

func rule(name, start, end string) *armpostgresqlflexibleservers.FirewallRule {
	return &armpostgresqlflexibleservers.FirewallRule{
		Name: to.Ptr(name),
		Properties: &armpostgresqlflexibleservers.FirewallRuleProperties{
			StartIPAddress: to.Ptr(start),
			EndIPAddress:   to.Ptr(end),
		},
	}
}

// The two rules the OBT blueprints deploy.
var (
	allowInternet      = rule("allow-all-internet", "0.0.0.0", "255.255.255.255")
	allowAzureServices = rule("allow-azure-services", "0.0.0.0", "0.0.0.0")
)

func TestCoversInternet(t *testing.T) {
	rules := []*armpostgresqlflexibleservers.FirewallRule{allowAzureServices, allowInternet}
	if !coversInternet(rules) {
		t.Error("coversInternet() = false with an allow-all-internet rule")
	}
	if coversInternet([]*armpostgresqlflexibleservers.FirewallRule{allowAzureServices}) {
		t.Error("coversInternet() = true with only the Azure-services marker")
	}
}

func TestCovers(t *testing.T) {
	address := netip.MustParseAddr("81.2.69.142")
	tests := []struct {
		name  string
		rules []*armpostgresqlflexibleservers.FirewallRule
		want  bool
	}{
		{"azure services only", []*armpostgresqlflexibleservers.FirewallRule{allowAzureServices}, false},
		{"exact", []*armpostgresqlflexibleservers.FirewallRule{rule("office", "81.2.69.142", "81.2.69.142")}, true},
		{"bracketing range", []*armpostgresqlflexibleservers.FirewallRule{rule("office", "81.2.69.0", "81.2.69.255")}, true},
		{"the range next door", []*armpostgresqlflexibleservers.FirewallRule{rule("office", "81.2.70.0", "81.2.70.255")}, false},
		{"nothing at all", nil, false},
		{"unparseable rule ignored", []*armpostgresqlflexibleservers.FirewallRule{rule("broken", "not-an-ip", "")}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := covers(test.rules, address); got != test.want {
				t.Errorf("covers() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRuleName(t *testing.T) {
	tests := []struct {
		user string
		want string
	}{
		{"ed@obtai.co.uk", "azd-db-ed-obtai-co-uk"},
		{"id-uks-dev-grid", "azd-db-id-uks-dev-grid"},
		{"@@@", "azd-db-user"},
	}
	for _, test := range tests {
		if got := ruleName(test.user); got != test.want {
			t.Errorf("ruleName(%q) = %q, want %q", test.user, got, test.want)
		}
	}
}
