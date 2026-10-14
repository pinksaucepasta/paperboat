//go:build darwin || linux

package workerupdate

import (
	"context"
	"testing"
	"time"
)

type timedObservationGate struct {
	fakeActivationGate
	extra    time.Duration
	calls    int
	window   time.Duration
	deadline time.Time
}

func (g *timedObservationGate) Active(ctx context.Context, r GateRequest) error {
	g.calls++
	g.window = r.Window
	g.deadline, _ = ctx.Deadline()
	timer := time.NewTimer(r.Window + g.extra)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func TestStabilityObservationAllowsBoundedCompletion(t *testing.T) {
	f := newFixture(t)
	f.candidate.StabilityWindow = 60 * time.Millisecond
	f.candidate.StabilityInterval = 10 * time.Millisecond
	gate := &timedObservationGate{extra: 20 * time.Millisecond}
	f.manager.config.Gate = gate
	if _, err := activatePrepared(context.Background(), f.manager, f.candidate); err != nil {
		t.Fatalf("healthy full-window observation failed: %v", err)
	}
	if gate.calls != 1 || gate.window != f.candidate.StabilityWindow {
		t.Fatalf("signed window changed: calls=%d window=%s", gate.calls, gate.window)
	}
}
