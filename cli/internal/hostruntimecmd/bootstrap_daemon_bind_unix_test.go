//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

// A daemon started at install, before pairing, is replaced at bind and must be seen serving.
func TestAwaitBoundDaemonBindsOnceTheReplacedDaemonServes(t *testing.T) {
	const version = "dev-test"
	probes := 0
	probe := func(context.Context) (localdaemon.UpdateProbe, error) {
		probes++
		if probes < 3 { // the old daemon is gone and the new one is not up yet
			return localdaemon.UpdateProbe{}, nil
		}
		return localdaemon.UpdateProbe{Running: true, Version: version, State: "starting"}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := awaitBoundDaemon(ctx, probe, version); err != nil {
		t.Fatal(err)
	}
}

// macOS pb pair, 2026-11-04: the new daemon was never loaded and pairing said only that binding failed.
func TestDaemonBindFailureNamesWhatTheProbeSaw(t *testing.T) {
	const version = "dev-test"
	for name, test := range map[string]struct {
		probe localdaemon.UpdateProbe
		err   error
		want  string
	}{
		"not running":   {want: "service is not running"},
		"other version": {probe: localdaemon.UpdateProbe{Running: true, Version: "dev-old", State: "ready"}, want: `version "dev-old"`},
		"no answer":     {err: errors.New("dial local API: connection refused"), want: "connection refused"},
		"degraded":      {probe: localdaemon.UpdateProbe{Running: true, Version: version, State: "degraded", Health: []string{"managed_ssh_unavailable"}, HealthTitles: []string{"Managed SSH is unavailable: the host key step failed"}}, want: "the host key step failed"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			err := awaitBoundDaemon(ctx, func(context.Context) (localdaemon.UpdateProbe, error) { return test.probe, test.err }, version)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err=%v", err)
			}
			message := runtimeFailureMessage(&installationStageError{Stage: "daemon_readiness", Cause: err})
			if !strings.Contains(message, "binding the local daemon") || !strings.Contains(message, test.want) || !strings.Contains(message, "rerun the same pair command") {
				t.Fatalf("message=%q", message)
			}
		})
	}
}
