package machineevents_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
)

// The wait loop reaching the control plane again after failing signals Recovered, so a consumer with its own long retry
// (the offline audit uploader) goes at once instead of sitting out its backoff. A loop that never failed does not signal.
func TestRecoveredSignalsWhenTheWaitLoopSucceedsAgainAfterFailing(t *testing.T) {
	var calls atomic.Int32
	wait := func(ctx context.Context, _ map[machineevents.Topic]int64) (map[machineevents.Topic]int64, error) {
		switch calls.Add(1) {
		case 1: // healthy first
			return map[machineevents.Topic]int64{machineevents.TopicMachineAccess: 1}, nil
		case 2: // the control plane becomes unreachable
			return nil, errors.New("dial: no route")
		default: // and comes back; then the long poll holds
			if calls.Load() == 3 {
				return map[machineevents.Topic]int64{machineevents.TopicMachineAccess: 2}, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	hub, err := machineevents.NewWithWaiter(wait, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	hub.Subscribe(machineevents.TopicMachineAccess)
	if err := hub.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hub.Shutdown(ctx)
	})
	select {
	case <-hub.Recovered():
	case <-time.After(10 * time.Second): // the loop's first backoff is about a second
		t.Fatalf("no recovery signal after %d waits", calls.Load())
	}
	select {
	case <-hub.Recovered():
		t.Fatal("a second signal without another failure")
	case <-time.After(200 * time.Millisecond):
	}
}
