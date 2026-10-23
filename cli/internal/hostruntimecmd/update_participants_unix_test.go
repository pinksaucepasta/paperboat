//go:build darwin || linux

package hostruntimecmd

import (
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
)

func TestUpdateProbeOutputBoundIncludesCopy(t *testing.T) {
	var output boundedUpdateProbeOutput
	if n, err := io.Copy(&output, strings.NewReader(strings.Repeat("x", 8<<10))); err != nil || n != 8<<10 {
		t.Fatalf("bounded output: %d %v", n, err)
	}
	if _, err := io.Copy(&output, strings.NewReader("x")); err == nil {
		t.Fatal("accepted oversized output")
	}
	if len(output.Bytes()) != 8<<10 {
		t.Fatal("oversized output retained")
	}
}

func TestUnixParticipantsConstructionBeforeInstallCommit(t *testing.T) {
	layout, err := service.UserLayout(runtime.GOOS, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// The constructor must not require install metadata: updater readiness is
	// established before the installer commits that metadata on a fresh install.
	if _, err := newUnixUpdateParticipants(layout.Binary, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := newUnixUpdateParticipants("/tmp/pb", 1000, 1000); err == nil {
		t.Fatal("accepted arbitrary executable")
	}
	if _, err := newUnixUpdateParticipants(layout.Binary, -1, 1000); err == nil {
		t.Fatal("accepted invalid owner")
	}
}

// An open SSH connection into this machine is carried by the carrier, not the user's daemon, so an
// update counts it too; either side that cannot answer only drops its own part.
func TestUpdateCountsSSHConnectionsIntoThisMachine(t *testing.T) {
	down := errors.New("unavailable")
	for _, test := range []struct {
		name                  string
		daemon                liveconnections.Counts
		daemonErr, carrierErr error
		incoming              int
		want                  liveconnections.Counts
		wantErr               bool
	}{
		{name: "incoming only", incoming: 1, want: liveconnections.Counts{SSH: 1}},
		{name: "both ends", daemon: liveconnections.Counts{SSH: 1, Forwards: 2}, incoming: 2, want: liveconnections.Counts{SSH: 3, Forwards: 2}},
		{name: "old carrier", daemon: liveconnections.Counts{Forwards: 1}, carrierErr: down, incoming: 5, want: liveconnections.Counts{Forwards: 1}},
		{name: "daemon down", daemonErr: down, daemon: liveconnections.Counts{SSH: 9}, incoming: 1, want: liveconnections.Counts{SSH: 1}},
		{name: "neither", daemonErr: down, carrierErr: down, wantErr: true},
	} {
		got, err := combineOpenConnections(test.daemon, test.daemonErr, test.incoming, test.carrierErr)
		if (err != nil) != test.wantErr || got != test.want {
			t.Fatalf("%s: %+v, %v", test.name, got, err)
		}
	}
}
