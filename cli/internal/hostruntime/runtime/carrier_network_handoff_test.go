//go:build linux || darwin || windows

package runtime

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A standby carrier (the update's candidate) prepares without the machine's
// state or network: StartNetwork waits for the serving carrier's release, then
// assembles and starts on the same identity; ReleaseNetwork stops it and
// frees the role. One carrier owns the network and its state at any time.
func TestStandbyCarrierStartsOnlyAfterTheServingCarrierReleasedTheRole(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "carrier-0.lock")
	unlockServing, err := lockCarrierFile(lock)
	if err != nil {
		t.Fatal(err)
	}
	var assembled, started atomic.Int32
	carrier := &ProductionCarrier{standby: true, roleLock: lock, assemble: func() (*servingCarrier, error) {
		assembled.Add(1)
		peer := &productionNativePeerService{}
		peer.startGeneration = func(context.Context) (*productionNativePeerGeneration, error) {
			started.Add(1)
			return &productionNativePeerGeneration{errors: make(chan error, 1)}, nil
		}
		return &servingCarrier{peer: peer, authorization: serviceGroup{}, usage: &hostBandwidthService{recorder: nil}}, nil
	}}
	if err := carrier.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if assembled.Load() != 0 || started.Load() != 0 {
		t.Fatal("a standby carrier touched state or the network while the serving carrier holds the role")
	}
	go func() { time.Sleep(200 * time.Millisecond); _ = unlockServing() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := carrier.StartNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	if assembled.Load() != 1 || started.Load() != 1 {
		t.Fatalf("after the release: assembled %d, network starts %d, want 1 and 1", assembled.Load(), started.Load())
	}
	if err := carrier.StartNetwork(ctx); err != nil || started.Load() != 1 {
		t.Fatal("a second start must not build another network", err, started.Load())
	}
	if err := carrier.ReleaseNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockCarrierFile(lock)
	if err != nil {
		t.Fatal("the role was not released with the network", err)
	}
	_ = unlock()
}
