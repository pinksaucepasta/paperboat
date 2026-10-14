package localdaemon

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func TestMachineAuthorityInvalidatorRefreshesChangedAndRemovedAuthorities(t *testing.T) {
	var invalidated []string
	observer := NewMachineAuthorityInvalidator(func(id string) { invalidated = append(invalidated, id) })
	machine := api.UserMachine{ID: "machine_1", EnvironmentID: "environment_1", PublicIdentityKey: "identity_1", InstallationGeneration: 3}
	if observer.Observe([]api.UserMachine{machine}) {
		t.Fatal("baseline reported a change")
	}
	machine.RuntimeDiagnostics.WorkerGeneration++
	machine.Online = true
	if observer.Observe([]api.UserMachine{machine}) || len(invalidated) != 0 {
		t.Fatalf("route-only change invalidated authority: %v", invalidated)
	}
	machine.InstallationGeneration++
	if !observer.Observe([]api.UserMachine{machine}) || len(invalidated) != 1 || invalidated[0] != machine.ID {
		t.Fatalf("generation change did not invalidate authority: %v", invalidated)
	}
	if !observer.Observe(nil) || len(invalidated) != 2 || invalidated[1] != machine.ID {
		t.Fatalf("removed machine did not invalidate authority: %v", invalidated)
	}
}
