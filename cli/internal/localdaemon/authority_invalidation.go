package localdaemon

import (
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// MachineAuthorityInvalidator drops cached authority when inventory changes
// the identity or generation used to authenticate a native peer.
type MachineAuthorityInvalidator struct {
	mu         sync.Mutex
	seen       map[string]machineAuthorityState
	ready      bool
	invalidate func(string)
}

type machineAuthorityState struct {
	environmentID   string
	identityKey     string
	installationGen int64
	terminalState   bool
}

func NewMachineAuthorityInvalidator(invalidate func(string)) *MachineAuthorityInvalidator {
	return &MachineAuthorityInvalidator{seen: make(map[string]machineAuthorityState), invalidate: invalidate}
}

// Observe establishes a baseline on the first inventory and invalidates only
// authorities that have changed or disappeared from a later inventory.
func (o *MachineAuthorityInvalidator) Observe(machines []api.UserMachine) bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	previous := o.seen
	current := make(map[string]machineAuthorityState, len(machines))
	for _, machine := range machines {
		current[machine.ID] = machineAuthorityState{
			environmentID: machine.EnvironmentID, identityKey: machine.PublicIdentityKey,
			installationGen: machine.InstallationGeneration,
			terminalState:   machine.State == "revoked" || machine.State == "deleted",
		}
	}
	var changed []string
	if o.ready {
		for id, old := range previous {
			if next, ok := current[id]; !ok || next != old {
				changed = append(changed, id)
			}
		}
	}
	o.seen, o.ready = current, true
	o.mu.Unlock()
	if o.invalidate != nil {
		for _, id := range changed {
			o.invalidate(id)
		}
	}
	return len(changed) > 0
}
