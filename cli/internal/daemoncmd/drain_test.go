package daemoncmd

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
)

type fakeDrainCoordinator struct {
	drained, listed atomic.Int32
}

func (c *fakeDrainCoordinator) Drain() error { c.drained.Add(1); return nil }
func (c *fakeDrainCoordinator) DrainingStreams() []string {
	c.listed.Add(1)
	return []string{"127.0.0.2:5432 -> machine_1:5432 (1 stream)"}
}

type fakeDrainIdentity struct {
	begun, withdrawn atomic.Int32
}

func (i *fakeDrainIdentity) BeginDrain(context.Context) error { i.begun.Add(1); return nil }
func (i *fakeDrainIdentity) Withdraw(context.Context) error   { i.withdrawn.Add(1); return nil }

func newTestDrainer(t *testing.T, limit time.Duration) (*drainer, *fakeDrainCoordinator, *fakeDrainIdentity, *atomic.Int32) {
	t.Helper()
	coordinator, identity := &fakeDrainCoordinator{}, &fakeDrainIdentity{}
	var stopped atomic.Int32
	d := newDrainer()
	d.coordinator, d.identity, d.stopLive, d.publish = coordinator, identity, func() { stopped.Add(1) }, func(int, time.Time) {}
	d.limit, d.poll = limit, 5*time.Millisecond
	return d, coordinator, identity, &stopped
}

func waitReason(t *testing.T, d *drainer) chan string {
	t.Helper()
	result := make(chan string, 1)
	go func() { result <- d.wait(t.Context()) }()
	return result
}

// SIGQUIT hands over every singleton once; the generation exits after its last forwarded stream
// closes and withdraws its network identity.
func TestDrainingGenerationExitsAfterItsLastStream(t *testing.T) {
	d, coordinator, identity, stopped := newTestDrainer(t, time.Hour)
	closeStream := liveconnections.TrackForward()
	for range 2 {
		if err := d.begin(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if coordinator.drained.Load() != 1 || stopped.Load() != 1 || identity.begun.Load() != 1 {
		t.Fatalf("handover ran drain=%d stopLive=%d beginDrain=%d, want once each", coordinator.drained.Load(), stopped.Load(), identity.begun.Load())
	}
	result := waitReason(t, d)
	select {
	case reason := <-result:
		t.Fatalf("drain ended (%s) while a stream was open", reason)
	case <-time.After(50 * time.Millisecond):
	}
	closeStream()
	if reason := <-result; reason != "drained" {
		t.Fatalf("reason = %s", reason)
	}
	if err := d.finish("drained"); err != nil || identity.withdrawn.Load() != 1 {
		t.Fatalf("withdraw = %v (%d)", err, identity.withdrawn.Load())
	}
}

// A stream held past runtimewire.DrainLimit does not keep the generation alive.
func TestDrainingGenerationExitsAtTheCap(t *testing.T) {
	d, _, _, _ := newTestDrainer(t, 30*time.Millisecond)
	defer liveconnections.TrackForward()()
	if reason := <-waitReason(t, d); reason != "drain_limit" {
		t.Fatalf("reason = %s", reason)
	}
}

// Revocation ends a drained stream at once and names it.
func TestRevocationEndsADrainedStream(t *testing.T) {
	d, coordinator, _, _ := newTestDrainer(t, time.Hour)
	defer liveconnections.TrackForward()()
	result := waitReason(t, d)
	d.identityRevoked("generation_evicted")
	d.identityRevoked("revoked") // a second report must not block
	if reason := <-result; reason != "generation_evicted" || coordinator.listed.Load() != 1 {
		t.Fatalf("reason = %s, streams listed %d times", reason, coordinator.listed.Load())
	}
}

// An exec running through the draining generation's local API keeps it alive like a forward does.
func TestDrainingGenerationWaitsForAnExecInFlight(t *testing.T) {
	d, _, _, _ := newTestDrainer(t, time.Hour)
	closeExec := liveconnections.TrackPeerStream()
	result := waitReason(t, d)
	select {
	case reason := <-result:
		t.Fatalf("drain ended (%s) while an exec was running", reason)
	case <-time.After(50 * time.Millisecond):
	}
	closeExec()
	if reason := <-result; reason != "drained" {
		t.Fatalf("reason = %s", reason)
	}
}
