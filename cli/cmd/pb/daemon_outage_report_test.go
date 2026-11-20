package main

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
)

func TestDaemonReportsControlPlaneUnavailableOnlyForItsOwnOutageHealth(t *testing.T) {
	cases := []struct {
		name     string
		snapshot localapi.Snapshot
		err      error
		want     bool
	}{
		{"outage", localapi.Snapshot{Health: []localapi.HealthItem{{Code: "control_plane_unavailable"}}}, nil, true},
		{"healthy", localapi.Snapshot{}, nil, false},
		{"other health", localapi.Snapshot{Health: []localapi.HealthItem{{Code: "authentication_required"}}}, nil, false},
		{"no daemon", localapi.Snapshot{}, errors.New("socket"), false},
	}
	for _, c := range cases {
		ctx := withHostDependencies(t.Context(), func(d *hostDependencies) {
			d.warmSnapshot = func(context.Context) (localapi.Snapshot, error) { return c.snapshot, c.err }
		})
		if got := daemonReportsControlPlaneUnavailable(ctx); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// During an outage exec resolves a machine the snapshot last saw offline (a restart looks like that) instead of
// asking the server for 30 s; outside an outage it still asks the server.
func TestWarmUserMachineTakesAnOfflineMachineOnlyDuringAnOutage(t *testing.T) {
	snapshot := localapi.Snapshot{Machines: []localapi.MachineStatus{{ID: "mch_a", Alias: "a", Eligible: true, RuntimeState: "offline", Generation: 3}}}
	ctx := withDaemonSnapshot(t, snapshot)
	if _, ok := warmUserMachine(ctx, "a", false); ok {
		t.Fatal("an offline machine was taken from the snapshot without an outage")
	}
	got, ok := warmUserMachine(ctx, "a", true)
	if !ok || got.ID != "mch_a" || got.InstallationGeneration != 3 {
		t.Fatalf("an offline machine was not taken from the snapshot during an outage: %+v %v", got, ok)
	}
}
