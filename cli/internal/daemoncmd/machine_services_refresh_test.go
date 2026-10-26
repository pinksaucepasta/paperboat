package daemoncmd

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
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
	go func() { refreshMachineServices(ctx, applier, fetch, changed, nudge); close(done) }()
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
