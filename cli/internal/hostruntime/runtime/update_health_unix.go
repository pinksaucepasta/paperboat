//go:build darwin || linux || windows

package runtime

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/availability"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

// updateHealthAvailability fills the availability observation's update health from the updater. The unix host
// service runs as root and cannot see the per-installation updater, so it reports "unknown"; doctor reads that as
// degraded on every machine. The Windows host service reports its own value, which is kept.
type updateHealthAvailability struct {
	source interface {
		Observation() *availability.Observation
	}
	updater interface {
		Status(context.Context) (updated.ControlResponse, error)
	}
}

func (a updateHealthAvailability) Observation(ctx context.Context) *availability.Observation {
	observation := a.source.Observation()
	if observation == nil || observation.UpdateHealth != "unknown" {
		return observation
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	status, err := a.updater.Status(probe)
	if err != nil {
		if ctx.Err() != nil {
			return observation // the caller ended (shutdown, send timeout), which says nothing about the updater
		}
		// An updater that cannot be reached needs a service restart, the same recovery doctor names.
		observation.UpdateHealth = "recovery_required"
		return observation
	}
	observation.UpdateHealth = status.UpdateHealth()
	return observation
}
