package daemoncmd

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// Leases are renewed shortly before the earliest one ends instead of every 8 s; changes arrive
// through the machine_services topic, so with no lease the daemon waits long.
func TestMachineServicesRenewBeforeTheEarliestLeaseEnds(t *testing.T) {
	now := time.Now()
	lease := func(remaining time.Duration) api.MachineServicesMachine {
		return api.MachineServicesMachine{ExpiresAt: now.Add(remaining)}
	}
	for name, test := range map[string]struct {
		services []api.MachineServicesMachine
		want     time.Duration
	}{
		"no lease":            {nil, machineServicesIdleRefresh},
		"fresh 30 s lease":    {[]api.MachineServicesMachine{lease(30 * time.Second)}, 20 * time.Second},
		"earliest of several": {[]api.MachineServicesMachine{lease(30 * time.Second), lease(18 * time.Second)}, 8 * time.Second},
		"about to end":        {[]api.MachineServicesMachine{lease(5 * time.Second)}, machineServicesMinimumRefresh},
		"already ended":       {[]api.MachineServicesMachine{lease(-time.Second)}, machineServicesMinimumRefresh},
	} {
		if got := nextMachineServicesRefresh(test.services, now); got != test.want {
			t.Errorf("%s: next refresh in %v, want %v", name, got, test.want)
		}
	}
}
