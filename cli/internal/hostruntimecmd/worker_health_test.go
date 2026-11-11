//go:build darwin || linux || windows

package hostruntimecmd

import (
	"context"
	"errors"
	"testing"
	"time"
)

type flakyWorkerFeature struct {
	failures int
	calls    int
}

func (f *flakyWorkerFeature) Health(context.Context) error {
	f.calls++
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
