package workerupdate

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
)

// MandatoryScheduler checks release availability only. Installation requires a
// separately prepared candidate, exact approval, and an explicit activation.
func (m *Manager) MandatoryScheduler(resolve Resolver, observe func(autoupdate.Observation)) (*autoupdate.Scheduler, error) {
	if resolve == nil {
		return nil, ErrInvalidConfig
	}
	return autoupdate.New(autoupdate.Config{
		Check: func(ctx context.Context) (autoupdate.Result, error) {
			result, err := m.Check(ctx, resolve)
			return autoupdate.Result{Version: result.Version, Updated: result.Updated}, err
		},
		Observe: observe,
	})
}
