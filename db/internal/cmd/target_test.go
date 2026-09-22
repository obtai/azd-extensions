package cmd

import (
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/postgresql/armpostgresqlflexibleservers/v4"
)

func serverID(group, name string) string {
	return "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/" + group +
		"/providers/Microsoft.DBforPostgreSQL/flexibleServers/" + name
}

// server is the Entra-only, public shape the OBT blueprints deploy.
func server(group, name string) *armpostgresqlflexibleservers.Server {
	return &armpostgresqlflexibleservers.Server{
		ID:   to.Ptr(serverID(group, name)),
		Name: to.Ptr(name),
		Properties: &armpostgresqlflexibleservers.ServerProperties{
			FullyQualifiedDomainName: to.Ptr(name + ".postgres.database.azure.com"),
			Version:                  to.Ptr(armpostgresqlflexibleservers.ServerVersionSixteen),
			Network: &armpostgresqlflexibleservers.Network{
				PublicNetworkAccess: to.Ptr(armpostgresqlflexibleservers.ServerPublicNetworkAccessStateEnabled),
			},
			AuthConfig: &armpostgresqlflexibleservers.AuthConfig{
				ActiveDirectoryAuth: to.Ptr(armpostgresqlflexibleservers.ActiveDirectoryAuthEnumEnabled),
				PasswordAuth:        to.Ptr(armpostgresqlflexibleservers.PasswordAuthEnumDisabled),
			},
		},
	}
}

func TestReachable(t *testing.T) {
	if err := reachable(server("rg-uks-dev-grid", "psql-uks-dev-grid")); err != nil {
		t.Fatalf("reachable() on a public Entra server = %v", err)
	}
}

func TestReachableRefusesAVNetServer(t *testing.T) {
	// The bootstrap blueprint's own shape: delegated subnet, no public endpoint.
	s := server("rg-uks-dev-plot", "psql-uks-dev-plot")
	s.Properties.Network.DelegatedSubnetResourceID = to.Ptr(
		"/subscriptions/x/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/snet-database")

	err := reachable(s)
	if err == nil {
		t.Fatal("reachable() on a VNet-injected server returned no error")
	}
	if !strings.Contains(err.Error(), "virtual network") || !strings.Contains(err.Error(), "azd shell") {
		t.Errorf("reachable() error = %q, want it to explain the VNet and name azd shell", err)
	}
}

func TestReachableRefusesPublicAccessDisabled(t *testing.T) {
	// CMS: public-access mode, but with access switched off.
	s := server("rg-cms", "psql-cms")
	s.Properties.Network.PublicNetworkAccess = to.Ptr(armpostgresqlflexibleservers.ServerPublicNetworkAccessStateDisabled)

	if err := reachable(s); err == nil {
		t.Error("reachable() with publicNetworkAccess disabled returned no error")
	}
}

func TestReachableRefusesPasswordAuth(t *testing.T) {
	s := server("rg-uks-dev-plot", "psql-uks-dev-plot")
	s.Properties.AuthConfig.ActiveDirectoryAuth = to.Ptr(armpostgresqlflexibleservers.ActiveDirectoryAuthEnumDisabled)
	s.Properties.AuthConfig.PasswordAuth = to.Ptr(armpostgresqlflexibleservers.PasswordAuthEnumEnabled)

	err := reachable(s)
	if err == nil {
		t.Fatal("reachable() on a password-auth server returned no error")
	}
	if !strings.Contains(err.Error(), "DB-PASSWORD") {
		t.Errorf("reachable() error = %q, want it to name the Key Vault secrets", err)
	}
}

func TestUserDatabases(t *testing.T) {
	databases := []*armpostgresqlflexibleservers.Database{
		{Name: to.Ptr("azure_maintenance")},
		{Name: to.Ptr("postgres")},
		{Name: to.Ptr("azure_sys")},
		{Name: to.Ptr("grid")},
		{Name: to.Ptr("grid_preview")},
		{Name: to.Ptr("grid_pr_95")},
	}
	got := userDatabases(databases)
	want := []string{"grid", "grid_preview", "grid_pr_95"}
	if !slices.Equal(got, want) {
		t.Errorf("userDatabases() = %v, want %v", got, want)
	}
}

func TestNamedDatabase(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]string
		want   string
	}{
		{"app prefix", map[string]string{
			"APP_DATABASE_NAME":         "grid",
			"APP_PREVIEW_DATABASE_NAME": "grid_preview",
		}, "grid"},
		{"sales prefix", map[string]string{"SALES_POSTGRES_DATABASE": "sales"}, "sales"},
		{"pg spelling", map[string]string{"PG_DATABASE": "plotdb"}, "plotdb"},
		{"nothing", map[string]string{"AZURE_RESOURCE_GROUP": "rg"}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := namedDatabase(test.values); got != test.want {
				t.Errorf("namedDatabase() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSortDatabases(t *testing.T) {
	names := []string{"grid_pr_95", "grid_preview", "grid"}
	sortDatabases(names, "grid")
	want := []string{"grid", "grid_pr_95", "grid_preview"}
	if !slices.Equal(names, want) {
		t.Errorf("sortDatabases() = %v, want %v", names, want)
	}
}

func TestSortDatabasesWithoutANamedOne(t *testing.T) {
	names := []string{"b", "c", "a"}
	sortDatabases(names, "")
	if !slices.Equal(names, []string{"a", "b", "c"}) {
		t.Errorf("sortDatabases() = %v, want alphabetical", names)
	}
}

func TestGroupsOf(t *testing.T) {
	servers := []*armpostgresqlflexibleservers.Server{
		server("rg-uks-prod-grid", "psql-prod"),
		server("rg-uks-dev-grid", "psql-dev"),
		server("RG-UKS-DEV-GRID", "psql-dev-2"),
	}
	got := groupsOf(servers)
	want := []string{"rg-uks-dev-grid", "rg-uks-prod-grid"}
	if !slices.Equal(got, want) {
		t.Errorf("groupsOf() = %v, want %v", got, want)
	}
}

func TestServerLabel(t *testing.T) {
	s := server("rg-uks-dev-grid", "psql-uks-dev-grid")
	if got, want := serverLabel(s, false), "psql-uks-dev-grid · PostgreSQL 16"; got != want {
		t.Errorf("serverLabel() = %q, want %q", got, want)
	}
	if got, want := serverLabel(s, true), "psql-uks-dev-grid · PostgreSQL 16 · rg-uks-dev-grid"; got != want {
		t.Errorf("serverLabel(withGroup) = %q, want %q", got, want)
	}
}
