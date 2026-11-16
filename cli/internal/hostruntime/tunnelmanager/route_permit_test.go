package tunnelmanager

import (
	"context"
	"testing"
	"time"
)

// F21-168: a stream offered to a route that is full for a moment (a second burst right after a near-full
// one) waits for a permit instead of being closed at once; a route that stays full still refuses it.
func TestAcquireRoutePermitWaitsForAPermitThatIsAboutToBeFree(t *testing.T) {
	permit := make(chan struct{}, 2)
	permit <- struct{}{}
	permit <- struct{}{}
	go func() {
		time.Sleep(50 * time.Millisecond)
		<-permit
	}()
	if !acquireRoutePermit(context.Background(), permit, 2*time.Second) {
		t.Fatal("a stream offered to a route freeing a permit within 50 ms was refused")
	}
	if len(permit) != 2 {
		t.Fatalf("%d permits held, want 2", len(permit))
	}
}

func TestAcquireRoutePermitRefusesARouteThatStaysFull(t *testing.T) {
	permit := make(chan struct{}, 1)
	permit <- struct{}{}
	started := time.Now()
	if acquireRoutePermit(context.Background(), permit, 50*time.Millisecond) {
		t.Fatal("a full route admitted a stream")
	}
	if waited := time.Since(started); waited < 40*time.Millisecond || waited > time.Second {
		t.Fatalf("refused after %s, want about the 50 ms wait", waited)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if acquireRoutePermit(ctx, permit, time.Hour) {
		t.Fatal("a canceled stream took a permit")
	}
}
