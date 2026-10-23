package main

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// Every command runs with a workspace selected ("personal" by default), so the warm snapshot must
// serve the personal workspace (the account's own personal machines) or no command ever uses it.
func TestPersonalWorkspaceResolvesOwnMachinesFromWarmSnapshot(t *testing.T) {
	machine := func(id, alias, ownership, owner string) localapi.MachineStatus {
		return localapi.MachineStatus{ID: id, EnvironmentID: "env_" + id, WorkspaceRoot: "/root", Alias: alias, Ownership: ownership, OwnerAccount: owner, Eligible: true, RuntimeState: "ready", Generation: 2, SelectedPath: "none", TransferReadiness: "unavailable", PreviewReadiness: "unavailable", SSHReadiness: "unavailable", NATMappingIPv4: "unknown", NATMappingIPv6: "unknown", CaptivePortal: "unknown", PMTU: "unknown", RouterProtocol: "unknown", RouterMapping: "unknown", MappingLifetime: "unknown", UpdateHealth: "unknown"}
	}
	snapshot := localapi.Snapshot{Schema: localapi.SnapshotSchemaV1, Generation: 1, ObservedAt: time.Now().UTC(), DaemonState: "ready", DaemonVersion: "dev", Machines: []localapi.MachineStatus{
		machine("mch_own", "build", "personal", "acct_me"),
		machine("mch_shared", "lent", "personal", "acct_other"),
		machine("mch_team", "ci", "team", "acct_me"),
	}}
	previous := loadWarmMachineSnapshot
	loadWarmMachineSnapshot = func(context.Context) (localapi.Snapshot, error) { return snapshot, nil }
	t.Cleanup(func() { loadWarmMachineSnapshot = previous })

	personal := context.WithValue(context.WithValue(context.Background(), workspaceInvocationKey{}, "personal"), personalWorkspaceAccountKey{}, "acct_me")
	if got, ok := resolveWarmUserMachine(personal, "build"); !ok || got.ID != "mch_own" || got.InstallationGeneration != 2 {
		t.Fatalf("own personal machine was not resolved warm: %+v %v", got, ok)
	}
	for _, alias := range []string{"lent", "ci"} {
		if got, ok := resolveWarmUserMachine(personal, alias); ok {
			t.Fatalf("%s is outside the personal workspace but resolved warm: %+v", alias, got)
		}
	}
	unknownAccount := context.WithValue(context.Background(), workspaceInvocationKey{}, "personal")
	if _, ok := resolveWarmUserMachine(unknownAccount, "build"); ok {
		t.Fatal("personal workspace without a known account resolved warm")
	}
	team := context.WithValue(context.Background(), workspaceInvocationKey{}, "team-a")
	if _, ok := resolveWarmUserMachine(team, "ci"); ok {
		t.Fatal("a team workspace resolved from the snapshot instead of the server")
	}
}

// pb status answered in ~1 s because it listed the account's machines on the server for every
// run; the unscoped and personal workspaces are answered from the daemon snapshot.
func TestStatusSelectsWorkspaceMachinesFromTheSnapshot(t *testing.T) {
	machines := []localapi.MachineStatus{
		{ID: "mch_own", Ownership: "personal", OwnerAccount: "acct_me"},
		{ID: "mch_shared", Ownership: "personal", OwnerAccount: "acct_other"},
		{ID: "mch_team", Ownership: "team", OwnerAccount: "acct_me"},
	}
	ids := func(selected []localapi.MachineStatus) []string {
		out := make([]string, 0, len(selected))
		for _, machine := range selected {
			out = append(out, machine.ID)
		}
		return out
	}
	if got, local := workspaceSnapshotMachines(context.Background(), machines); !local || len(got) != 3 {
		t.Fatalf("unscoped status = %v local=%v, want every snapshot machine", ids(got), local)
	}
	personal := context.WithValue(context.WithValue(context.Background(), workspaceInvocationKey{}, "personal"), personalWorkspaceAccountKey{}, "acct_me")
	if got, local := workspaceSnapshotMachines(personal, machines); !local || len(got) != 1 || got[0].ID != "mch_own" {
		t.Fatalf("personal status = %v local=%v, want only mch_own", ids(got), local)
	}
	team := context.WithValue(context.Background(), workspaceInvocationKey{}, "team-a")
	if got, local := workspaceSnapshotMachines(team, machines); local {
		t.Fatalf("team workspace answered from the snapshot: %v", ids(got))
	}
}
