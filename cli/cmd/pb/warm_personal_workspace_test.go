package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
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

// A warm `pb ping --count 1` took ~2 s for a 5 ms connection: it listed every machine on the
// server (~1 s) before the daemon probe. A machine the warm snapshot sees ready needs no listing.
func TestPingResolvesAWarmMachineWithoutListing(t *testing.T) {
	snapshot := localapi.Snapshot{Schema: localapi.SnapshotSchemaV1, Generation: 1, ObservedAt: time.Now().UTC(), DaemonState: "ready", DaemonVersion: "dev", Machines: []localapi.MachineStatus{
		{ID: "mch_ready", EnvironmentID: "env_ready", Alias: "build", Eligible: true, RuntimeState: "ready", Generation: 2},
	}}
	previous := loadWarmMachineSnapshot
	loadWarmMachineSnapshot = func(context.Context) (localapi.Snapshot, error) { return snapshot, nil }
	t.Cleanup(func() { loadWarmMachineSnapshot = previous })
	listings := 0
	errListed := errors.New("listed on the server")
	server := func() (*api.Client, error) { listings++; return nil, errListed }

	machine, err := resolveMachineWarmFirst(context.Background(), "build", server)
	if err != nil || machine.ID != "mch_ready" || machine.EnvironmentID != "env_ready" || machine.InstallationGeneration != 2 || !machine.Online {
		t.Fatalf("warm machine = %+v, %v", machine, err)
	}
	if listings != 0 {
		t.Fatalf("a warm machine was listed on the server %d times", listings)
	}
	if _, err := resolveMachineWarmFirst(context.Background(), "unknown", server); !errors.Is(err, errListed) || listings != 1 {
		t.Fatalf("a machine outside the snapshot was not resolved on the server: err=%v listings=%d", err, listings)
	}
}
