package daemoncmd

import (
	"context"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/diagnosticlog"
)

// Announced-service leases last only as long as a host keeps announcing (the server's snapshot
// lifetime, 30 s), so the daemon renews them shortly before the earliest one ends, and refreshes at
// once when the server's machine_services topic says a snapshot changed.
const (
	machineServicesRenewBefore    = 10 * time.Second
	machineServicesMinimumRefresh = 2 * time.Second
	// machineServicesIdleRefresh bounds the wait when no lease is held: a missed topic still
	// shows up within it.
	machineServicesIdleRefresh  = 5 * time.Minute
	machineServicesTimeout      = 10 * time.Second
	machineServicesMaximumRetry = 30 * time.Second
)

type serviceApplier interface {
	ApplyServices(context.Context, []api.MachineServicesMachine) error
}

// nextMachineServicesRefresh is how long to wait before renewing the listed leases.
func nextMachineServicesRefresh(services []api.MachineServicesMachine, now time.Time) time.Duration {
	wait := machineServicesIdleRefresh
	for _, service := range services {
		wait = min(wait, service.ExpiresAt.Sub(now)-machineServicesRenewBefore)
	}
	return max(wait, machineServicesMinimumRefresh)
}

func refreshMachineServices(ctx context.Context, coordinator serviceApplier, serverURL string, auth *sessionauth.Source, transport http.RoundTripper, changed <-chan struct{}) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	retry := machineServicesMinimumRefresh
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-changed:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		stepCtx, cancel := context.WithTimeout(ctx, machineServicesTimeout)
		credential, err := auth.WithContext(stepCtx).Credential()
		var services []api.MachineServicesMachine
		if err == nil {
			if services, err = api.New(serverURL, credential, &http.Client{Transport: transport}).MachineServices(stepCtx); err == nil {
				err = coordinator.ApplyServices(stepCtx, services)
			}
		}
		cancel()
		wait := nextMachineServicesRefresh(services, time.Now())
		if err == nil {
			retry = machineServicesMinimumRefresh
		} else {
			// A failing server is retried with doubling waits, not at the renewal cadence.
			wait, retry = retry, min(retry*2, machineServicesMaximumRetry)
			if ctx.Err() == nil {
				diagnosticlog.TryWarn("could not renew the announced-service leases; the next attempt follows", "error", err)
			}
		}
		timer.Reset(wait)
	}
}
