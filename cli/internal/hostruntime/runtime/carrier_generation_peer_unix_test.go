//go:build linux || darwin

package runtime

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
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

// A delegated generation takes the primary role only once the holder released
// it, only while it is the active generation, and never once retired.
func TestDelegatedCarrierTakesThePrimaryRoleOnlyWhenFreeAndActive(t *testing.T) {
	lock := t.TempDir() + "/carrier-0.lock"
	holder, err := lockTerminalCarrier(lock)
	if err != nil {
		t.Fatal(err)
	}
	var active atomic.Bool
	peer := &productionNativePeerService{promote: make(chan struct{}, 1), config: productionNativePeerConfig{carrierGeneration: "gen"}}
	c := &ProductionCarrier{peer: peer, slot: 1, primaryLock: lock, active: func(context.Context) (bool, error) { return active.Load(), nil }, takeoverDone: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go c.awaitPrimary(ctx)
	promoted := func(within time.Duration) bool {
		select {
		case <-peer.promote:
			return true
		case <-time.After(within):
			return false
		}
	}
	if promoted(2 * primaryTakeoverInterval) {
		t.Fatal("took the primary role while the old primary held it")
	}
	if err := holder(); err != nil {
		t.Fatal(err)
	}
	if promoted(2 * primaryTakeoverInterval) {
		t.Fatal("an inactive generation took the primary role")
	}
	active.Store(true)
	if !promoted(3 * primaryTakeoverInterval) {
		t.Fatal("the active generation did not take the free primary role")
	}
	<-c.takeoverDone
	if peer.config.carrierGeneration != "" {
		t.Fatal("a later restart would build a delegated generation again")
	}
	if _, err := lockTerminalCarrier(lock); err == nil {
		t.Fatal("the primary role is not held after the takeover")
	}
	if err := c.unlockPrimary(); err != nil {
		t.Fatal(err)
	}

	retired := &ProductionCarrier{peer: &productionNativePeerService{promote: make(chan struct{}, 1)}, slot: 1, primaryLock: lock, active: c.active, takeoverDone: make(chan struct{})}
	retired.peer.retired.Store(true)
	go retired.awaitPrimary(ctx)
	select {
	case <-retired.takeoverDone:
	case <-time.After(3 * primaryTakeoverInterval):
		t.Fatal("a retired generation kept waiting for the primary role")
	}
	if len(retired.peer.promote) != 0 {
		t.Fatal("a retired generation took the primary role")
	}
}

// Offer reports never block the generation and publish only the latest offer;
// a renewal that raced the promotion's withdrawal never republishes an offer.
func TestCarrierGenerationReportsKeepTheLatestAndNeverFollowTheWithdrawal(t *testing.T) {
	g := &productionNativePeerGeneration{reports: make(chan native.CarrierOffer, 1)}
	first, renewed := native.CarrierOffer{Signature: []byte{1}}, native.CarrierOffer{Signature: []byte{2}}
	g.reportCarrierGeneration(first)
	g.reportCarrierGeneration(renewed) // the reporter is still busy: this must not block
	if got := <-g.reports; len(got.Signature) != 1 || got.Signature[0] != 2 {
		t.Fatalf("reported %v, want the renewed offer only", got.Signature)
	}
	g.reportCarrierGeneration(native.CarrierOffer{})
	g.reportCarrierGeneration(renewed)
	if got := <-g.reports; len(got.Signature) != 0 {
		t.Fatal("a renewal replaced the withdrawal")
	}
	g.reportCarrierGeneration(renewed)
	select {
	case got := <-g.reports:
		t.Fatalf("an offer %v was reported after the withdrawal", got.Signature)
	default:
	}
}

// A retired generation keeps its held stream, admits new connections only for
// the overlap, and exits once its last stream ends, or at its cap.
func TestRetiredGenerationDrainsHeldStreamsPastTheOverlap(t *testing.T) {
	c := &ProductionCarrier{peer: &productionNativePeerService{}, ssh: &managedssh.Host{}}
	c.peer.streams.Add(1)
	done := make(chan error, 1)
	go func() { done <- c.drain(context.Background(), 100*time.Millisecond, 10*time.Second) }()
	time.Sleep(20 * time.Millisecond)
	if c.peer.refuseNew.Load() {
		t.Fatal("new connections refused during the overlap")
	}
	time.Sleep(300 * time.Millisecond)
	if !c.peer.refuseNew.Load() {
		t.Fatal("new connections still admitted after the overlap")
	}
	select {
	case err := <-done:
		t.Fatal("drain cut a held stream", err)
	default:
	}
	c.peer.streams.Add(-1)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the generation did not exit after its last stream")
	}

	capped := &ProductionCarrier{peer: &productionNativePeerService{}, ssh: &managedssh.Host{}}
	capped.peer.streams.Add(1)
	if err := capped.drain(context.Background(), time.Hour, 300*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) || !capped.peer.refuseNew.Load() {
		t.Fatal("drain outlived its cap or kept admitting", err)
	}
}
