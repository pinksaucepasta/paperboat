package hostinstall

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

func TestLocalDaemonServingAcceptsASignedOutDaemonOnlyOnAnEnrolledMachine(t *testing.T) {
	signedOut := localapi.Snapshot{DaemonVersion: "v2", DaemonState: "awaiting_enrollment", Health: []localapi.HealthItem{{Code: "authentication_required"}}}
	for name, c := range map[string]struct {
		state      localapi.Snapshot
		registered bool
		want       bool
	}{
		"ready":                          {localapi.Snapshot{DaemonVersion: "v2", DaemonState: "ready"}, true, true},
		"signed out, enrolled machine":   {signedOut, true, true},
		"signed out, no registration":    {signedOut, false, false},
		"awaiting without a sign-out":    {localapi.Snapshot{DaemonVersion: "v2", DaemonState: "awaiting_enrollment"}, true, false},
		"older daemon still answering":   {localapi.Snapshot{DaemonVersion: "v1", DaemonState: "ready"}, true, false},
		"degraded is not serving":        {localapi.Snapshot{DaemonVersion: "v2", DaemonState: "degraded"}, true, false},
		"signed out but another version": {localapi.Snapshot{DaemonVersion: "v1", DaemonState: "awaiting_enrollment", Health: signedOut.Health}, true, false},
	} {
		if got := localDaemonServing(c.state, "v2", c.registered); got != c.want {
			t.Errorf("%s: localDaemonServing = %v, want %v", name, got, c.want)
		}
	}
}
