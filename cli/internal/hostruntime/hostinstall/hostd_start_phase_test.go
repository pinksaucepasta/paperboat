//go:build darwin || linux || windows

package hostinstall

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostdproto"
)

// A readiness failure names the step the host service was in, one distinct answer per step of its start.
func TestHostdStartPhaseNamesEachStepOfTheStart(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	pipe := errors.New("pipe not found")
	for name, test := range map[string]struct {
		status hostdproto.Status
		err    error
		want   string
	}{
		"owner not serving":  {err: pipe, want: "service does not answer yet"},
		"no worker":          {status: hostdproto.Status{State: hostdproto.StateEmpty}, want: "its runtime has not registered"},
		"worker registered":  {status: hostdproto.Status{State: hostdproto.StateCandidate, WorkerID: "runtime-1"}, want: "runtime runtime-1 registered but was not activated"},
		"runtime starting":   {status: hostdproto.Status{State: hostdproto.StateActive, WorkerID: "runtime-1"}, want: "has not finished starting"},
		"runtime not served": {status: hostdproto.Status{State: hostdproto.StateActive, WorkerID: "runtime-1", LastHeartbeatUnixMilli: now.Add(-7 * time.Second).UnixMilli()}, want: "last heartbeat 7s ago"},
	} {
		got := hostdStartPhase(test.status, test.err, now)
		if got == nil || !strings.Contains(got.Error(), test.want) || (test.err != nil) != errors.Is(got, pipe) {
			t.Errorf("%s: phase = %v, want it to contain %q", name, got, test.want)
		}
	}
}
