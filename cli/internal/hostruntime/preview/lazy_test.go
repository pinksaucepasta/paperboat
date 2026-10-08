package preview

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLazyLifecycleWaitsForLastStreamThenStopsIdleLease(t *testing.T) {
	now := time.Now().UTC()
	lifecycle := newLazyLifecycle(context.Background(), func() time.Time { return time.Now().UTC() }, 25*time.Millisecond, time.Hour, now.Add(time.Hour))
	streamCtx, closeStream := lifecycle.BeginStream(context.Background(), now.Add(time.Hour))
	select {
	case <-lifecycle.Done():
		t.Fatal("active stream did not hold lazy lease")
	case <-time.After(50 * time.Millisecond):
	}
	closeStream()
	select {
	case <-lifecycle.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("last stream close did not start idle cleanup")
	}
	if !errors.Is(streamCtx.Err(), context.Canceled) {
		t.Fatalf("stream context after close = %v", streamCtx.Err())
	}
}

func TestLazyLifecycleCapsStreamByAuthorization(t *testing.T) {
	now := time.Now().UTC()
	lifecycle := newLazyLifecycle(context.Background(), func() time.Time { return time.Now().UTC() }, time.Hour, time.Hour, now.Add(time.Hour))
	streamCtx, closeStream := lifecycle.BeginStream(context.Background(), now.Add(25*time.Millisecond))
	defer closeStream()
	select {
	case <-streamCtx.Done():
		if !errors.Is(streamCtx.Err(), context.DeadlineExceeded) {
			t.Fatalf("stream error = %v", streamCtx.Err())
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("authorization deadline did not cap stream")
	}
}

func TestSessionRenewalPreservesDaemonLocalLazyLifecycle(t *testing.T) {
	now := time.Now().UTC()
	lifecycle := newLazyLifecycle(context.Background(), func() time.Time { return time.Now().UTC() }, time.Hour, time.Hour, now.Add(time.Hour))
	defer lifecycle.Stop()
	previous := sessionTestLease(LeaseRequest{OwnerMachineID: "machine_1", OwnerSessionID: "session_01234567-89ab-4cde-8fab-0123456789ab", OwnerSessionKind: "lazy_runtime", Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, AccessMode: "team"})
	previous.ETag = formatLeaseETag(previous.ID, 1)
	previous.Generation = 1
	previous.LazyLifecycle = lifecycle
	renewed := previous
	renewed.ETag = formatLeaseETag(previous.ID, 2)
	renewed.Generation = 0
	renewed.LazyLifecycle = nil // HTTP decoding cannot recreate daemon-local state.
	session := &Session{config: SessionConfig{OwnerMachineID: previous.OwnerMachineID, OwnerSessionID: previous.OwnerSessionID, OwnerSessionKind: previous.OwnerSessionKind, Target: previous.Target, AccessMode: previous.AccessMode, DisableParentWatch: true, Now: func() time.Time { return now }}, lease: previous, cancel: func() {}}
	if err := session.acceptRenewal(renewed, previous); err != nil {
		t.Fatal(err)
	}
	if session.currentLease().LazyLifecycle != lifecycle {
		t.Fatal("renewal discarded lazy lifecycle")
	}
}
