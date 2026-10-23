//go:build darwin || linux

package hostruntimecmd

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

// Pairing waited for the new daemon to be "ready" and reported "Machine pairing failed while binding the
// local daemon" when its first inventory refresh could not reach a slow control plane, though the machine
// was bound and worked (pbvm2/pbvm3 containers, 2026-10-22).
func TestBoundDaemonServesWhenOnlyTheControlPlaneIsUnreachable(t *testing.T) {
	const version = "dev-test"
	for name, test := range map[string]struct {
		probe localdaemon.UpdateProbe
		want  bool
	}{
		"ready":                     {localdaemon.UpdateProbe{Running: true, Version: version, State: "ready"}, true},
		"control plane unreachable": {localdaemon.UpdateProbe{Running: true, Version: version, State: "degraded", ControlPlaneUnavailableOnly: true}, true},
		"degraded otherwise":        {localdaemon.UpdateProbe{Running: true, Version: version, State: "degraded"}, false},
		"still starting":            {localdaemon.UpdateProbe{Running: true, Version: version, State: "starting"}, true},
		"not enrolled":              {localdaemon.UpdateProbe{Running: true, Version: version, State: "awaiting_enrollment"}, false},
		"old daemon":                {localdaemon.UpdateProbe{Running: true, Version: "dev-old", State: "ready"}, false},
		"not running":               {localdaemon.UpdateProbe{Version: version, State: "ready"}, false},
	} {
		if got := boundDaemonServing(test.probe, version); got != test.want {
			t.Errorf("%s: serving=%t, want %t", name, got, test.want)
		}
	}
}

// Pairing must wait at least for the new daemon's first refresh, or a slow control plane makes a
// successful pair report failure while the daemon still says "starting".
func TestPairingWaitsForTheDaemonsFirstRefresh(t *testing.T) {
	if daemonBindWindow < localdaemon.FirstRefreshBudget+20*time.Second {
		t.Fatalf("bind window %v leaves no room for service start before the daemon's first refresh (%v)", daemonBindWindow, localdaemon.FirstRefreshBudget)
	}
}
