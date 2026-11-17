package health

import (
	"time"

	"github.com/pinksaucepasta/paperboat/shared/health"
)

// NewHealthTracker returns the host runtime's typed health tracker.
func NewHealthTracker(now func() time.Time) (*health.HealthTracker, error) {
	return health.NewHealthTracker(now, "All applicable host health dimensions are ready.")
}
