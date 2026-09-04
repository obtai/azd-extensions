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

// WaitHealthy blocks until an app's latest revision reports healthy, and
// deactivates it otherwise so the previous revision keeps serving.
func WaitHealthy(ctx context.Context, clients *Clients, app string) error {
	response, err := clients.ContainerApps.Get(ctx, clients.ResourceGroup, app, nil)
	if err != nil {
		return err
	}
	if response.Properties == nil || response.Properties.LatestRevisionName == nil {
		fmt.Printf("==> postdeploy: %s has no revisions yet\n", app)
		return nil
	}
	latest := *response.Properties.LatestRevisionName

	fmt.Printf("==> postdeploy: waiting for %s\n", latest)

	state := "Unknown"
	healthy := Until(ctx, healthTimeout, healthInterval, func(ctx context.Context) bool {
		revision, err := clients.Revisions.GetRevision(ctx, clients.ResourceGroup, app, latest, nil)
		if err != nil {
			return false
		}
		if revision.Properties != nil && revision.Properties.HealthState != nil {
			state = string(*revision.Properties.HealthState)
		}
		if state != "Healthy" {
			fmt.Printf("    %s…\n", state)
		}
		return state == "Healthy"
	})

	if !healthy {
		_, _ = clients.Revisions.DeactivateRevision(ctx, clients.ResourceGroup, app, latest, nil)
		return fmt.Errorf("%s never became healthy (%s)", latest, state)
	}

	fmt.Printf("==> %s is healthy\n", latest)
	return nil
}
