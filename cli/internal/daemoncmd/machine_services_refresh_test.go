package daemoncmd

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

type countingServiceApplier struct{ applied atomic.Int32 }

func (a *countingServiceApplier) ApplyServices(context.Context, []api.MachineServicesMachine) error {
	a.applied.Add(1)
	return nil
}

// An idle daemon fetches the service list once and then only on the machine_services topic or a
// nudge, not before every short server lease ends (Lane W: one GET every ~7 s per idle machine).
func TestMachineServicesAreFetchedOnlyOnTopicOrNudge(t *testing.T) {
	var fetches atomic.Int32
	fetch := func(context.Context) ([]api.MachineServicesMachine, error) {
		fetches.Add(1)
		// Leases far shorter than the test: the old loop renewed them every 2 s minimum.
		return []api.MachineServicesMachine{{MachineID: "m", ExpiresAt: time.Now().Add(50 * time.Millisecond)}}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	changed, nudge := make(chan struct{}), make(chan struct{})
	applier := &countingServiceApplier{}
	done := make(chan struct{})
	go func() {
		refreshMachineServices(ctx, health.New("test", nil, nil), applier, fetch, changed, nudge)
		close(done)
	}()
	waitFor := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for applier.applied.Load() < want && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := applier.applied.Load(); got != want {
			t.Fatalf("applied %d lists, want %d", got, want)
		}
	}
	waitFor(1)
	time.Sleep(2500 * time.Millisecond)
	if got := fetches.Load(); got != 1 {
		t.Fatalf("an idle daemon fetched machine services %d times in 2.5 s, want 1", got)
	}
	changed <- struct{}{}
	waitFor(2)
	nudge <- struct{}{}
	waitFor(3)
	cancel()
	<-done
}

// A failed service-list fetch is retried on its own with backoff (no topic, nudge or restart needed) and is
// the failing machine_services step until then.
func TestMachineServicesFailureRetriesAndReportsUntilItRecovers(t *testing.T) {
	var fetches atomic.Int32
	release := make(chan struct{})
	fetch := func(context.Context) ([]api.MachineServicesMachine, error) {
		if fetches.Add(1) == 1 {
			return nil, errors.New("server unavailable")
		}
		<-release
		return nil, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	steps := health.New("test", nil, nil)
	applier := &countingServiceApplier{}
	done := make(chan struct{})
	go func() { refreshMachineServices(ctx, steps, applier, fetch, nil, nil); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for fetches.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	failing := steps.Snapshot().Capabilities[localdaemon.StepMachineServices]
	if fetches.Load() != 2 || failing.State != health.Degraded || failing.Reason != "server unavailable" || failing.RetryAfterMs == 0 {
		t.Fatalf("after one failure: fetches=%d step=%+v, want a retry with the step failing", fetches.Load(), failing)
	}
	close(release)
	// Ready is set after the apply, so wait for the step itself.
	for steps.Snapshot().Capabilities[localdaemon.StepMachineServices].State != health.Ready && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := steps.Snapshot().Capabilities[localdaemon.StepMachineServices]; applier.applied.Load() != 1 || got.State != health.Ready {
		t.Fatalf("after the retry: applied=%d step=%+v, want ready", applier.applied.Load(), got)
	}
	cancel()
	<-done
}
