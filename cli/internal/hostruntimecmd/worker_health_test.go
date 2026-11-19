//go:build darwin || linux || windows

package hostruntimecmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/freeze"
)

type flakyWorkerFeature struct {
	failures int
	frozen   bool
	calls    int
}

func (f *flakyWorkerFeature) Health(context.Context) error {
	f.calls++
	if f.calls == 1 && f.frozen {
		freeze.SimulateStall(30 * time.Second)
	}
	if f.calls <= f.failures {
		return errors.New("connection reset by peer")
	}
	return nil
}
func (*flakyWorkerFeature) Shutdown(context.Context) error { return nil }

// One failed health call (a connection to the session owner reset at its idle close) no longer ends the worker,
// and hostd with it; an owner that fails again right after still does (canary-a 2026-11-11 19:33).
func TestWorkerSurvivesOneFailedHealthCheck(t *testing.T) {
	previous := workerHealthRetry
	workerHealthRetry = time.Millisecond
	defer func() { workerHealthRetry = previous }()
	once := &flakyWorkerFeature{failures: 1}
	if err := workerFeatureHealthy(t.Context(), once); err != nil || once.calls != 2 {
		t.Fatalf("one failed health check ended the worker: %v after %d calls", err, once.calls)
	}
	twice := &flakyWorkerFeature{failures: 2}
	if err := workerFeatureHealthy(t.Context(), twice); err == nil {
		t.Fatal("an owner that stays unreachable was tolerated")
	}
}

// The terminal-owner health call that fails because the whole process was frozen (SIGSTOP 134 s on pbvm2) is
// tried again with a fresh window instead of ending the worker and hostd with it.
func TestWorkerSurvivesAHealthCheckThatFailedOnAFreeze(t *testing.T) {
	previous := workerHealthRetry
	workerHealthRetry = time.Millisecond
	defer func() { workerHealthRetry = previous }()
	frozen := &flakyWorkerFeature{failures: 2, frozen: true}
	if err := workerFeatureHealthy(t.Context(), frozen); err != nil || frozen.calls != 3 {
		t.Fatalf("a freeze ended the worker: %v after %d calls", err, frozen.calls)
	}
}
