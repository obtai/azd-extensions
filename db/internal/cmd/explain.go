package cmd

import (
	"context"
	"slices"
	"strings"

	"github.com/obtai/azd-extensions/db/internal/azure"
	"github.com/obtai/azd-extensions/db/internal/ui"
)

// psqlConnectionFailure is psql's exit status for "could not connect", as
// against 1 for a bad query and 3 for a script error.
const psqlConnectionFailure = 2

// explain adds what psql cannot know. A server that has no role for you says
// "password authentication failed", which is a baffling thing to read when no
// password was sent — the token was fine, there is simply nobody to be.
func explain(ctx context.Context, clients *azure.Clients, resourceGroup, server, user string) {
	admins, err := clients.Administrators(ctx, resourceGroup, server)
	if err != nil || len(admins) == 0 {
		return
	}
	if slices.ContainsFunc(admins, func(admin string) bool { return strings.EqualFold(admin, user) }) {
		// The role exists and the login still failed: psql's own message is
		// the better one, whatever it said.
		return
	}
	// Being absent from this list is not proof — an admin can create a role for
	// any principal with pgaadauth_create_principal — so this is phrased as the
	// likely cause rather than the verdict.
	ui.Note("%s is not a Microsoft Entra administrator of %s, and may have no role there.\n"+
		"  Administrators: %s\n"+
		"  One of them can make you a role:\n"+
		"    select * from pgaadauth_create_principal('%s', false, false);",
		user, server, strings.Join(admins, ", "), user)
}
