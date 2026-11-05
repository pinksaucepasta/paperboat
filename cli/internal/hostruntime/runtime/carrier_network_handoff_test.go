//go:build linux || darwin

package runtime

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A standby carrier (the update's candidate) prepares without the machine's
// network: StartNetwork waits for the serving carrier's release, then brings
// the network up on the same identity; ReleaseNetwork takes it down and frees
// the primary role. One identity is on the network at any time.
func TestStandbyCarrierStartsItsNetworkOnlyAfterTheServingCarrierReleased(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "carrier-0.lock")
	unlockServing, err := lockTerminalCarrier(lock)
	if err != nil {
		t.Fatal(err)
	}
	var started atomic.Int32
	peer := &productionNativePeerService{}
	peer.startGeneration = func(context.Context) (*productionNativePeerGeneration, error) {
		started.Add(1)
		return &productionNativePeerGeneration{errors: make(chan error, 1)}, nil
	}
	carrier := &ProductionCarrier{peer: peer, authorization: serviceGroup{}, standby: true, primaryLock: lock}
	if err := carrier.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if started.Load() != 0 {
		t.Fatal("a standby carrier started the network while the serving carrier holds it")
	}
	go func() { time.Sleep(200 * time.Millisecond); _ = unlockServing() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := carrier.StartNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	if started.Load() != 1 {
		t.Fatalf("network starts after the release = %d, want 1", started.Load())
	}
	if err := carrier.StartNetwork(ctx); err != nil || started.Load() != 1 {
		t.Fatal("a second start must not build another network", err, started.Load())
	}
	if err := carrier.ReleaseNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockTerminalCarrier(lock)
	if err != nil {
		t.Fatal("the primary role was not released with the network", err)
	}
	_ = unlock()
}
