//go:build darwin || linux

package workerupdate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
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
	if _, err := f.manager.Activate(context.Background(), f.candidate); err != nil {
		t.Fatalf("healthy full-window observation failed: %v", err)
	}
	if gate.calls != 1 || gate.window != f.candidate.StabilityWindow {
		t.Fatalf("signed window changed: calls=%d window=%s", gate.calls, gate.window)
	}
}
func TestRecoveredStabilityKeepsPersistedCompletionDeadline(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "remaining", true: "expired"}[expired], func(t *testing.T) {
			f := newFixture(t)
			f.candidate.StabilityWindow = 100 * time.Millisecond
			f.candidate.StabilityInterval = 10 * time.Millisecond
			gate := &timedObservationGate{}
			f.manager.config.Gate = gate
			f.hostd.active = hostdproto.Status{State: hostdproto.StateActive, WorkerID: workerID(f.candidate.Version), Epoch: 2, APIVersion: 1}
			j := withRelease(f.manager.newJournal(), f.candidate, f.paths.staged)
			j.Stage = updateflow.StageMonitoring
			j.WorkerID = f.hostd.active.WorkerID
			j.WorkerEpoch = 2
			// The nominal window ended; only the existing bounded completion margin remains.
			j.HealthDeadline = time.Now().Add(-time.Second + 40*time.Millisecond)
			if expired {
				j.HealthDeadline = time.Now().Add(-2 * time.Second)
			}
			if err := f.manager.write(j); err != nil {
				t.Fatal(err)
			}
			persisted, err := updateflow.Load(f.paths.journal)
			if err != nil {
				t.Fatal(err)
			}
			err = f.manager.monitor(context.Background(), persisted, f.candidate)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expired observation accepted: %v", err)
			}
			if expired {
				if gate.calls != 0 {
					t.Fatal("expired recovery started another observation")
				}
				return
			}
			if gate.window != f.candidate.StabilityWindow || gate.deadline.Sub(j.HealthDeadline.Add(time.Second)).Abs() > 5*time.Millisecond {
				t.Fatalf("recovery extended persisted bound or shortened window: window=%s deadline=%s expected=%s", gate.window, gate.deadline, j.HealthDeadline.Add(time.Second))
			}
		})
	}
}
