package azure

import (
	"context"
	"fmt"
	"time"
)

const (
	healthTimeout  = 10 * time.Minute
	healthInterval = 5 * time.Second
)

// WaitHealthy blocks until a revision reports healthy, and deactivates it
// otherwise so it stops holding replicas and a revision slot.
//
// The authoritative check. An HTTP probe against the label's FQDN proves
// routing as well, but it cannot tell a revision that is slow from one that will
// never start, so it can only ever warn — this is what fails.
func WaitHealthy(ctx context.Context, clients *Clients, app, revision string) error {
	fmt.Printf("==> Waiting for %s\n", revision)

	state := "Unknown"
	healthy := Until(ctx, healthTimeout, healthInterval, func(ctx context.Context) bool {
		response, err := clients.Revisions.GetRevision(ctx, clients.ResourceGroup, app, revision, nil)
		if err != nil {
			return false
		}
		if response.Properties != nil && response.Properties.HealthState != nil {
			state = string(*response.Properties.HealthState)
		}
		if state != "Healthy" {
			fmt.Printf("    %s…\n", state)
		}
		return state == "Healthy"
	})

	if !healthy {
		_, _ = clients.Revisions.DeactivateRevision(ctx, clients.ResourceGroup, app, revision, nil)
		return fmt.Errorf("%s never became healthy (%s), and has been deactivated", revision, state)
	}

	fmt.Printf("    %s is healthy\n", revision)
	return nil
}
