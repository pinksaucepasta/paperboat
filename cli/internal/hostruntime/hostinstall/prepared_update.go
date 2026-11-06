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
	// Bind builds Stage and Validate from the request the installation really runs with. A supplied
	// offline bundle has no source until the privileged helper has verified it, so the caller cannot know
	// the target version when it builds the hooks.
	Bind func(Request) (stage, validate func(context.Context) error)
}

// bound returns the hooks with Stage and Validate built for the resolved request.
func (h PreparedUpdateHooks) bound(request Request) PreparedUpdateHooks {
	if h.Bind != nil {
		h.Stage, h.Validate = h.Bind(request)
	}
	return h
}
