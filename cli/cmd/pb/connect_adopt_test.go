package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

type fakeAdopter struct {
	failures atomic.Int32
	calls    atomic.Int32
	adopted  atomic.Int32
}

func (f *fakeAdopter) AdoptOfflineTerminalSession(_ context.Context, machineID, sessionID string) (api.TerminalSession, error) {
	f.calls.Add(1)
	if f.failures.Add(-1) >= 0 {
		return api.TerminalSession{}, errors.New("machine is offline")
	}
	f.adopted.Add(1)
	return api.TerminalSession{}, nil
}

// An offline session is registered as soon as Paperboat answers, after earlier refusals, and only once.
func TestOfflineSessionIsAdoptedOnceTheMachineIsOnline(t *testing.T) {
	f := &fakeAdopter{}
	f.failures.Store(2)
	finish := adoptOfflineSession(context.Background(), f, "machine_1", "terminal_offline_a", 5*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for f.adopted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	finish()
	if f.adopted.Load() != 1 || f.calls.Load() != 3 {
		t.Fatalf("adopted %d times in %d calls, want once in 3", f.adopted.Load(), f.calls.Load())
	}
}

// A connect that ends before the machine answered tries once more on the way out.
func TestOfflineSessionGetsOneLastTryWhenTheConnectEnds(t *testing.T) {
	f := &fakeAdopter{}
	f.failures.Store(0)
	finish := adoptOfflineSession(context.Background(), f, "machine_1", "terminal_offline_a", time.Hour)
	finish()
	if f.adopted.Load() != 1 {
		t.Fatalf("adopted %d times, want the last try to register it", f.adopted.Load())
	}
}
