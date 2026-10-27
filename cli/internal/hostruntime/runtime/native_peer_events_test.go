//go:build darwin || linux || windows

package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents/eventstest"
)

// A peer joining, leaving or changing keys is published by the server; the machine renews its signed
// peer network at once instead of at the next 30 s renewal, on the running engine: a restart gave the engine
// a new port and dropped every open session (hp 2026-10-27, a preview create broke pb preview inspect).
func TestNativePeerRenewsItsNetworkWhenThePeersChange(t *testing.T) {
	_, subscriptions, plane := eventstest.Start(t, time.Hour, machineevents.TopicPeerNetwork)
	var refreshes, starts atomic.Int32
	service := &productionNativePeerService{config: productionNativePeerConfig{networkEvents: subscriptions[0]}}
	service.startGeneration = func(context.Context) (*productionNativePeerGeneration, error) {
		starts.Add(1)
		return &productionNativePeerGeneration{cancel: func() {}, errors: make(chan error, 1), requestRefresh: func() { refreshes.Add(1) }}, nil
	}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer service.Shutdown(context.Background())
	time.Sleep(100 * time.Millisecond)
	if refreshes.Load() != 0 {
		t.Fatalf("renewed without a change: %d", refreshes.Load())
	}
	plane.Publish(machineevents.TopicPeerNetwork)
	deadline := time.Now().Add(2 * time.Second)
	for refreshes.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a peer change did not renew the signed network")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(productionNativePeerRestartDelay + 200*time.Millisecond)
	if got := starts.Load(); got != 1 {
		t.Fatalf("a peer change restarted the native peer engine: %d generations", got)
	}
}
