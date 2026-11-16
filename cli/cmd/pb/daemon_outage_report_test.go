package main

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

func TestDaemonReportsControlPlaneUnavailableOnlyForItsOwnOutageHealth(t *testing.T) {
	previous := loadWarmMachineSnapshot
	t.Cleanup(func() { loadWarmMachineSnapshot = previous })
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
		loadWarmMachineSnapshot = func(context.Context) (localapi.Snapshot, error) { return c.snapshot, c.err }
		if got := daemonReportsControlPlaneUnavailable(context.Background()); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
