package telemetry

import (
	"time"

	"github.com/pinksaucepasta/paperboat/shared/health"
)

// NewHealthTracker returns the edge's health tracker.
func NewHealthTracker(now func() time.Time) (*health.HealthTracker, error) {
	return health.NewHealthTracker(now, "All applicable edge health dimensions are ready.")
}
