package route

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// TestTRK35GenerationHandoffForceDrainsOld exercises the failure ordering that
// matters during a hot route replacement: a ready replacement is promoted
// before a bounded drain is forced by cancellation.
func TestTRK35GenerationHandoffForceDrainsOld(t *testing.T) {
	clock := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	registry, err := NewGenerationRegistryWithOptions(GenerationRegistryOptions{
		TelemetryClock: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	oldRule := matcherRule("route_old", "app.example.test", MatchExact, "/", 1)
	newRule := matcherRule("route_new", "app.example.test", MatchExact, "/", 1)
	activateMatcher(t, registry, 1, []RouteRule{oldRule})

	oldLease, oldMatch, err := registry.Acquire(context.Background(), "app.example.test", "/")
	if err != nil || oldMatch.Generation != 1 {
		t.Fatalf("old lease = %+v, err=%v", oldMatch, err)
	}
	defer oldLease.Close()

	if err := registry.StageGeneration(2, []RouteRule{newRule}); err != nil {
		t.Fatal(err)
	}
	if err := registry.MarkGenerationReady(2); err != nil {
		t.Fatal(err)
	}
	activationContext, cancelActivation := context.WithCancel(context.Background())
	activationDone := make(chan error, 1)
	go func() {
		activationDone <- registry.ActivateGeneration(activationContext, 2, time.Hour)
	}()

	// Activation publishes the new generation before it waits for old leases.
	waitContext, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	for registry.Generation() != 2 {
		select {
		case <-waitContext.Done():
			t.Fatal("ready replacement was not promoted before drain")
		default:
			runtime.Gosched()
		}
	}
	match, err := registry.Match("app.example.test", "/")
	if err != nil || match.Generation != 2 || match.Rule.ID != newRule.ID {
		t.Fatalf("replacement match = %+v, err=%v", match, err)
	}
	// The new generation serves streams while the old generation is draining.
	newLease, _, err := registry.Acquire(context.Background(), "app.example.test", "/")
	if err != nil {
		t.Fatalf("replacement acquire = %v", err)
	}
	defer newLease.Close()

	cancelActivation()
	select {
	case err := <-activationDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("forced drain error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forced drain did not return")
	}
	select {
	case <-oldLease.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("forced drain did not cancel old lease")
	}
	if got := registry.Generation(); got != 2 {
		t.Fatalf("forced drain replaced active generation with %d, want 2", got)
	}
}
