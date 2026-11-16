package machineevents

import (
	"context"
	"testing"
	"time"
)

func TestPushedEventsRecoverAndFallBackWithoutWorker(t *testing.T) {
	hub := NewPushed()
	sub := hub.Subscribe(TopicPeerNetwork)
	if err := hub.Start(t.Context()); err != ErrInvalid {
		t.Fatalf("local receiver started a server wait: %v", err)
	}
	if sub.Healthy() {
		t.Fatal("absent worker marked healthy")
	}
	// An absent worker must retain the consumer's timer.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if !sub.Wait(ctx, time.Millisecond) {
		t.Fatal("absent worker disabled fallback")
	}
	hub.Push(nil, true)
	if !sub.Healthy() {
		t.Fatal("healthy push not received")
	}
	select {
	case <-hub.Recovered():
	default:
		t.Fatal("offline audit missed recovery")
	}
	<-sub.Changed() // initial recovery refresh
	hub.Push(nil, true)
	select {
	case <-sub.Changed():
		t.Fatal("heartbeat refreshed unchanged network")
	default:
	}
	hub.Push([]Topic{TopicPeerNetwork}, true)
	select {
	case <-sub.Changed():
	default:
		t.Fatal("network push missed")
	}
	hub.mu.Lock()
	hub.lastPush = time.Now().Add(-PushHealthLifetime)
	hub.mu.Unlock()
	if sub.Healthy() {
		t.Fatal("dead worker's health lease never expired")
	}
	if !sub.Wait(ctx, time.Millisecond) {
		t.Fatal("lost worker disabled fallback")
	}
	hub.Push(nil, true)
	select {
	case <-hub.Recovered():
	default:
		t.Fatal("returning worker did not wake audit")
	}
	hub.Push(nil, false)
	if sub.Healthy() {
		t.Fatal("worker stream failure hidden")
	}
}
