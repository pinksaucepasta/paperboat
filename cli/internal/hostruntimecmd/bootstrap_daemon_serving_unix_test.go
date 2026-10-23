//go:build darwin || linux

package hostruntimecmd

import (
	"context"
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

// C020 (round 9, hp): `pb setup` returned while its daemon was still starting, so `pb doctor` right
// after it said degraded. Pairing waits, bounded, for a bound daemon to leave "starting", and says so
// when it has not instead of claiming the runtime is ready.
func TestPairingWaitsForAStartingDaemonToSettle(t *testing.T) {
	probeAfter := func(states ...string) func(context.Context) (localdaemon.UpdateProbe, error) {
		calls := 0
		return func(context.Context) (localdaemon.UpdateProbe, error) {
			state := states[min(calls, len(states)-1)]
			calls++
			return localdaemon.UpdateProbe{Running: true, Version: "dev-test", State: state}, nil
		}
	}
	if got := pairedMessage(t.Context(), probeAfter("starting", "starting", "ready")); got != "Paperboat machine runtime is ready." {
		t.Fatalf("a daemon that becomes ready: %q", got)
	}
	if !awaitBoundDaemonSettled(t.Context(), probeAfter("starting", "degraded")) {
		t.Fatal("a degraded daemon has settled; pb status and pb doctor show why")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if awaitBoundDaemonSettled(ctx, probeAfter("starting")) {
		t.Fatal("a daemon that is still starting has not settled")
	}
	if ctx.Err() == nil {
		t.Fatal("the wait must last until its bound")
	}
	if daemonSettleWindow > 2*time.Minute {
		t.Fatalf("settle window %v is not bounded tightly enough for an interactive pair", daemonSettleWindow)
	}
}
