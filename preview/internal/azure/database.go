package azure

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/postgresql/armpostgresqlflexibleservers/v4"
)

// EnsureDatabase creates a database on a flexible server over ARM, or reports
// that it already exists.
//
// ARM rather than psql: a server behind a private endpoint cannot be reached
// from a runner at all, and an app that migrates itself at boot — Plot runs
// its migrations when the api starts — needs nothing more than an empty
// database with the right name. This is the one door that is always open.
func (c *Clients) EnsureDatabase(ctx context.Context, server, name string) (created bool, err error) {
	_, err = c.Databases.Get(ctx, c.ResourceGroup, server, name, nil)
	if err == nil {
		return false, nil
	}
	if !NotFound(err) {
		return false, err
	}

	poller, err := c.Databases.BeginCreate(ctx, c.ResourceGroup, server, name,
		armpostgresqlflexibleservers.Database{
			Properties: &armpostgresqlflexibleservers.DatabaseProperties{
				Charset:   to.Ptr("UTF8"),
				Collation: to.Ptr("en_US.utf8"),
			},
		}, nil)
	if err != nil {
		return false, fmt.Errorf("creating database %s on %s: %w", name, server, err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return false, fmt.Errorf("creating database %s on %s: %w", name, server, err)
	}
	return true, nil
}

// DropDatabase deletes a database, treating "already gone" as done.
func (c *Clients) DropDatabase(ctx context.Context, server, name string) (dropped bool, err error) {
	_, err = c.Databases.Get(ctx, c.ResourceGroup, server, name, nil)
	if err != nil {
		if NotFound(err) {
			return false, nil
		}
		return false, err
	}

	poller, err := c.Databases.BeginDelete(ctx, c.ResourceGroup, server, name, nil)
	if err != nil {
		return false, err
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return false, err
	}
	return true, nil
}

// DatabaseExists is the read behind `status`.
func (c *Clients) DatabaseExists(ctx context.Context, server, name string) (bool, error) {
	_, err := c.Databases.Get(ctx, c.ResourceGroup, server, name, nil)
	if err == nil {
		return true, nil
	}
	if NotFound(err) {
		return false, nil
	}
	return false, err
}
