package main

import (
	"errors"
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

func TestRegisteredSSHTargetInFindsTheMachineInTheInventoryList(t *testing.T) {
	machines := []api.UserMachine{
		{ID: "machine_other", SSH: &api.MachineSSH{OSUser: "other", Port: 2222, TargetGeneration: 1}},
		{ID: "machine_this", SSH: &api.MachineSSH{OSUser: "runneradmin", Port: 58965, TargetGeneration: 1}},
	}
	if got, found := registeredSSHTargetIn(machines, "machine_this", 1); !found || got.OSUser != "runneradmin" || got.Port != 58965 {
		t.Fatalf("the machine's own target was not found: %+v %v", got, found)
	}
	if got, found := registeredSSHTargetIn(machines, "machine_missing", 1); found || got.Port != 0 {
		t.Fatalf("a machine that is not listed gave a target: %+v %v", got, found)
	}
}

// The inventory list is personal-only: a team-enrolled machine is missing from it and was left with HOST\\user and the
// default port (the 409 again); it is read by id instead.
func TestResolveRegisteredSSHTargetReadsATeamEnrolledMachineById(t *testing.T) {
	team := api.UserMachine{ID: "machine_team", SSH: &api.MachineSSH{OSUser: "runneradmin", Port: 58965, TargetGeneration: 1}}
	list := func() ([]api.UserMachine, error) { return []api.UserMachine{{ID: "machine_personal"}}, nil }
	gets := 0
	get := func(id string) (api.UserMachine, error) {
		gets++
		if id != "machine_team" {
			t.Fatalf("read the wrong machine: %s", id)
		}
		return team, nil
	}
	got, err := resolveRegisteredSSHTarget("machine_team", 1, list, get)
	if err != nil || got.OSUser != "runneradmin" || got.Port != 58965 || gets != 1 {
		t.Fatalf("a team-enrolled machine's target was not read by id: %+v err=%v gets=%d", got, err, gets)
	}
	listed := func() ([]api.UserMachine, error) { return []api.UserMachine{team}, nil }
	if _, err := resolveRegisteredSSHTarget("machine_team", 1, listed, get); err != nil || gets != 1 {
		t.Fatalf("a listed machine was read by id again: err=%v gets=%d", err, gets)
	}
	failing := func(string) (api.UserMachine, error) { return api.UserMachine{}, errors.New("denied") }
	if _, err := resolveRegisteredSSHTarget("machine_team", 1, list, failing); err == nil {
		t.Fatal("a failed read was hidden")
	}
}
