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
