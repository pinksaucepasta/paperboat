package hostinstall

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

// PreparedUpdateHooks carries the verified signed deployment policy around
// native replacement. Prepare follows workload admission and precedes service
// stops; Validate runs after native readiness and before source metadata commit.
type PreparedUpdateHooks struct {
	// Stage runs after verified binary promotion, before workload admission pauses.
	Stage            func(context.Context) error
	DrainTimeout     time.Duration
	Prepare          func(context.Context) error
	Validate         func(context.Context) error
	AuthorizeRestore func(context.Context, installsource.Source, string) error
}
