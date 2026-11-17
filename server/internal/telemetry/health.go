package telemetry

import (
	"time"

	"github.com/pinksaucepasta/paperboat/shared/health"
)

// NewHealthTracker returns the control plane's health tracker.
func NewHealthTracker(now func() time.Time) (*health.HealthTracker, error) {
	return health.NewHealthTracker(now, "All applicable control-plane health dimensions are ready.")
}
