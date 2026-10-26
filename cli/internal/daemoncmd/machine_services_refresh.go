package daemoncmd

import (
	"context"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/shared/backoff"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// The announced-service list is fetched when the server's machine_services topic says a snapshot
// changed (a port opened or closed), on a local nudge, and otherwise rarely. The server's 30 s
// snapshot lease is not renewed from here: a host renews it by announcing, a content-identical
// renewal moves no topic, and a host that goes away leaves the inventory, which withdraws its
// ports. Renewing before the earliest lease of every machine in the account cost one GET every
// ~7 s per idle daemon (Lane W, F21-19).
const (
	// machineServicesIdleRefresh bounds the wait between topics: a missed topic still shows up
	// within it.
	machineServicesIdleRefresh  = 5 * time.Minute
	machineServicesTimeout      = 10 * time.Second
	machineServicesFirstRetry   = 2 * time.Second
	machineServicesMaximumRetry = 30 * time.Second
)

type serviceApplier interface {
	ApplyServices(context.Context, []api.MachineServicesMachine) error
}

type machineServicesFetch func(context.Context) ([]api.MachineServicesMachine, error)

func machineServicesFetcher(serverURL string, auth *sessionauth.Source, transport http.RoundTripper) machineServicesFetch {
	return func(ctx context.Context) ([]api.MachineServicesMachine, error) {
		credential, err := auth.WithContext(ctx).Credential()
		if err != nil {
			return nil, err
		}
		return api.New(serverURL, credential, &http.Client{Transport: transport}).MachineServices(ctx)
	}
}

func refreshMachineServices(ctx context.Context, coordinator serviceApplier, fetch machineServicesFetch, changed, nudge <-chan struct{}) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	retry := machineServicesFirstRetry
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-nudge:
			stopTimer(timer)
		case <-changed:
			stopTimer(timer)
		}
		stepCtx, cancel := context.WithTimeout(ctx, machineServicesTimeout)
		services, err := fetch(stepCtx)
		if err == nil {
			err = coordinator.ApplyServices(stepCtx, services)
		}
		cancel()
		wait := machineServicesIdleRefresh
		if err == nil {
			retry = machineServicesFirstRetry
		} else {
			// A failing server is retried with doubling waits.
			wait, retry = backoff.Jitter(retry), min(retry*2, machineServicesMaximumRetry)
			if ctx.Err() == nil {
				diagnosticlog.TryWarn("could not refresh the announced services; the next attempt follows", "error", err)
			}
		}
		timer.Reset(wait)
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
