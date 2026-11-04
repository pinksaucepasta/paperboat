//go:build linux || darwin

package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Each running carrier generation owns one slot's state files; a fifth
// concurrent generation is refused instead of sharing a writer.
func TestCarrierSlotsAreExclusiveAndBounded(t *testing.T) {
	root := t.TempDir()
	unlocks := make([]func() error, carrierSlots)
	for want := range carrierSlots {
		slot, unlock, err := lockCarrierSlot(root)
		if err != nil || slot != want {
			t.Fatalf("slot = %d, %v; want %d", slot, err, want)
		}
		unlocks[want] = unlock
	}
	if _, _, err := lockCarrierSlot(root); !errors.Is(err, ErrCarrierSlotsBusy) {
		t.Fatal("a fifth generation shared a slot", err)
	}
	if err := unlocks[1](); err != nil {
		t.Fatal(err)
	}
	slot, unlock, err := lockCarrierSlot(root)
	if err != nil || slot != 1 {
		t.Fatalf("freed slot = %d, %v; want 1", slot, err)
	}
	for i, release := range append(unlocks, unlock) {
		if i != 1 {
			_ = release()
		}
	}
}

// A retired generation waits for its open streams, and gives up at its limit.
func TestCarrierDrainWaitsForOpenStreams(t *testing.T) {
	s := &productionNativePeerService{}
	s.streams.Add(1)
	done := make(chan error, 1)
	go func() { done <- s.drainStreams(context.Background(), 10*time.Second) }()
	select {
	case err := <-done:
		t.Fatal("drain returned with a stream open", err)
	case <-time.After(400 * time.Millisecond):
	}
	s.streams.Add(-1)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s.streams.Add(1)
	if err := s.drainStreams(context.Background(), 300*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("drain outlived its limit", err)
	}
}
