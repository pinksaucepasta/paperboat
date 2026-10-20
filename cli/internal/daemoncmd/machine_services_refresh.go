package daemoncmd

import (
	"context"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/diagnosticlog"
)

// machineServicesRefreshInterval is shorter than the lifetime of an announced-service lease, so
// the forwarded ports of a machine stay up between full inventory refreshes.
const machineServicesRefreshInterval = 8 * time.Second

type serviceApplier interface {
	ApplyServices(context.Context, []api.MachineServicesMachine) error
}

func refreshMachineServices(ctx context.Context, coordinator serviceApplier, serverURL string, auth *sessionauth.Source, transport http.RoundTripper) {
	ticker := time.NewTicker(machineServicesRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		stepCtx, cancel := context.WithTimeout(ctx, machineServicesRefreshInterval)
		credential, err := auth.WithContext(stepCtx).Credential()
		if err == nil {
			var services []api.MachineServicesMachine
			if services, err = api.New(serverURL, credential, &http.Client{Transport: transport}).MachineServices(stepCtx); err == nil {
				err = coordinator.ApplyServices(stepCtx, services)
			}
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			diagnosticlog.TryWarn("could not renew the announced-service leases; the next attempt follows", "error", err)
		}
	}
}
