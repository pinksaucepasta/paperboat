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
