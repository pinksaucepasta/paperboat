package main

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// The kept target is the server's registration, so a stale or planted local registration file cannot pick it.
func TestKeptWindowsSSHTargetIsTheServersRegistrationOrTheCallersValues(t *testing.T) {
	registered := api.MachineSSH{OSUser: "runneradmin", Port: 58965}
	if user, port := keptWindowsSSHTarget(registered, `HOST\runneradmin`, 38222); user != "runneradmin" || port != 58965 {
		t.Fatalf("the server's registered target was replaced: %q %d", user, port)
	}
	for name, none := range map[string]api.MachineSSH{
		"no registration": {},
		"no port":         {OSUser: "alice"},
		"no user":         {Port: 58965},
	} {
		if user, port := keptWindowsSSHTarget(none, "bob", 38222); user != "bob" || port != 38222 {
			t.Fatalf("%s replaced the caller's values: %q %d", name, user, port)
		}
	}
}

func TestSameWindowsAccountComparesTheBareNameOfTheDomainForm(t *testing.T) {
	if !sameWindowsAccount("runneradmin", `RUNNERVM\RunnerAdmin`) || !sameWindowsAccount("alice", "alice") {
		t.Fatal("the same account was reported as another")
	}
	if sameWindowsAccount("alice", `HOST\bob`) {
		t.Fatal("another account was reported as the same")
	}
}

func TestRegisteredSSHTargetIsTheServersTargetOfThisGenerationOnly(t *testing.T) {
	listed := api.UserMachine{SSH: &api.MachineSSH{OSUser: "runneradmin", Port: 58965, TargetGeneration: 1}}
	if got := registeredSSHTarget(listed, 1); got.OSUser != "runneradmin" || got.Port != 58965 {
		t.Fatalf("the server's target of this generation was not used: %+v", got)
	}
	if got := registeredSSHTarget(listed, 2); got.OSUser != "" || got.Port != 0 {
		t.Fatalf("a target of an older generation was kept: %+v", got)
	}
	if got := registeredSSHTarget(api.UserMachine{}, 1); got.OSUser != "" || got.Port != 0 {
		t.Fatalf("a machine without a target gave one: %+v", got)
	}
}
