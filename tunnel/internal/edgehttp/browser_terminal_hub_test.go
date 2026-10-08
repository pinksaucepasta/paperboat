package edgehttp

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestBrowserTerminalHubFansOutToAtMost100Participants(t *testing.T) {
	hub := NewBrowserTerminalHub()
	defer hub.Close()
	identity := testEdgePreviewIdentity(1, 1)
	fence := BrowserTerminalRouteFence{Identity: identity, Revision: 1, AttachmentGeneration: 1}
	if err := hub.ActivateRoute("route_test", fence); err != nil {
		t.Fatal(err)
	}
	key := BrowserTerminalSessionKey{RouteID: "route_test", TerminalSessionID: "term_1", ProcessGeneration: identity.ProcessGeneration}
	subscribers := make([]*BrowserTerminalSubscription, 0, browserTerminalMaxParticipants)
	for i := range browserTerminalMaxParticipants {
		subscriber, err := hub.Subscribe(key, fence, "attach_"+strconv.Itoa(i+1))
		if err != nil {
			t.Fatalf("participant %d: %v", i+1, err)
		}
		subscribers = append(subscribers, subscriber)
	}
	if _, err := hub.Subscribe(key, fence, "attach_101"); !errors.Is(err, ErrBrowserTerminalHubFull) {
		t.Fatalf("101st participant error = %v", err)
	}
	publisher, err := hub.RegisterPublisher(key, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	if _, err := hub.RegisterPublisher(key, identity); !errors.Is(err, ErrBrowserTerminalHubBusy) {
		t.Fatalf("second publisher error = %v", err)
	}
	const record = "opaque-signed-and-encrypted-output"
	if err := publisher.Publish([]byte(record)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for i, subscriber := range subscribers {
		got, err := subscriber.Next(ctx)
		if err != nil || string(got) != record {
			t.Fatalf("participant %d got %q, err=%v", i+1, got, err)
		}
		subscriber.Close()
	}
}

func TestBrowserTerminalPublisherLossClosesBrowsersForFreshScreen(t *testing.T) {
	hub := NewBrowserTerminalHub()
	defer hub.Close()
	identity := testEdgePreviewIdentity(1, 1)
	fence := BrowserTerminalRouteFence{Identity: identity, Revision: 1, AttachmentGeneration: 1}
	if err := hub.ActivateRoute("route_test", fence); err != nil {
		t.Fatal(err)
	}
	key := BrowserTerminalSessionKey{RouteID: "route_test", TerminalSessionID: "term_1", ProcessGeneration: identity.ProcessGeneration}
	old, err := hub.Subscribe(key, fence, "browser_old")
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := hub.RegisterPublisher(key, identity)
	if err != nil {
		t.Fatal(err)
	}
	publisher.Close()
	select {
	case <-old.Done():
	case <-time.After(time.Second):
		t.Fatal("browser stayed attached after output feed ended")
	}
	if _, err := old.Next(context.Background()); !errors.Is(err, ErrBrowserTerminalHubEnded) {
		t.Fatalf("old browser still receives output: %v", err)
	}
	fresh, err := hub.Subscribe(key, fence, "browser_fresh")
	if err != nil {
		t.Fatal(err)
	}
	next, err := hub.RegisterPublisher(key, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if err := next.Publish([]byte("fresh encrypted record")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if record, err := fresh.Next(ctx); err != nil || string(record) != "fresh encrypted record" {
		t.Fatalf("fresh browser record=%q error=%v", record, err)
	}
}

func TestBrowserTerminalHubDropsOutputWithoutParticipantsAndBoundsRecords(t *testing.T) {
	hub := NewBrowserTerminalHub()
	defer hub.Close()
	identity := testEdgePreviewIdentity(1, 1)
	fence := BrowserTerminalRouteFence{Identity: identity, Revision: 1, AttachmentGeneration: 1}
	if err := hub.ActivateRoute("route_test", fence); err != nil {
		t.Fatal(err)
	}
	key := BrowserTerminalSessionKey{RouteID: "route_test", TerminalSessionID: "term_1", ProcessGeneration: identity.ProcessGeneration}
	publisher, err := hub.RegisterPublisher(key, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	if err := publisher.Publish([]byte("no subscriber; discard")); err != nil {
		t.Fatalf("empty-session output was retained as an error: %v", err)
	}
	subscriber, err := hub.Subscribe(key, fence, "attach_1")
	if err != nil {
		t.Fatal(err)
	}
	defer subscriber.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := subscriber.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("subscriber received output from before it joined: %v", err)
	}
	if err := publisher.Publish(bytes.Repeat([]byte{0xA5}, browserTerminalMaxRecordBytes)); err != nil {
		t.Fatalf("maximum COSE record rejected: %v", err)
	}
	if err := publisher.Publish(bytes.Repeat([]byte{0xA5}, browserTerminalMaxRecordBytes+1)); !errors.Is(err, ErrBrowserTerminalHubInvalid) {
		t.Fatalf("oversized COSE record error = %v", err)
	}
}

func TestBrowserTerminalHubDisconnectsOnlySlowParticipantAndFencesOldGeneration(t *testing.T) {
	hub := NewBrowserTerminalHub()
	defer hub.Close()
	identity := testEdgePreviewIdentity(1, 1)
	fence := BrowserTerminalRouteFence{Identity: identity, Revision: 1, AttachmentGeneration: 1}
	if err := hub.ActivateRoute("route_test", fence); err != nil {
		t.Fatal(err)
	}
	key := BrowserTerminalSessionKey{RouteID: "route_test", TerminalSessionID: "term_1", ProcessGeneration: identity.ProcessGeneration}
	slow, err := hub.Subscribe(key, fence, "attach_slow")
	if err != nil {
		t.Fatal(err)
	}
	fast, err := hub.Subscribe(key, fence, "attach_fast")
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := hub.RegisterPublisher(key, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	if err := publisher.Publish([]byte("first")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := fast.Next(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < browserTerminalQueuedRecords; i++ {
		if err := publisher.Publish([]byte("second")); err != nil {
			t.Fatal(err)
		}
		if got, err := fast.Next(ctx); err != nil || string(got) != "second" {
			t.Fatalf("fast participant at %d got %q, err=%v", i, got, err)
		}
	}
	select {
	case <-slow.Done():
	case <-ctx.Done():
		t.Fatal("slow participant was not disconnected")
	}
	if err := publisher.Publish([]byte("after slow participant")); err != nil {
		t.Fatal(err)
	}
	if got, err := fast.Next(ctx); err != nil || string(got) != "after slow participant" {
		t.Fatalf("fast participant got %q, err=%v", got, err)
	}
	if _, err := slow.Next(ctx); !errors.Is(err, ErrBrowserTerminalHubEnded) {
		t.Fatalf("slow participant remained active: %v", err)
	}

	identity2 := testEdgePreviewIdentity(2, 2)
	fence2 := BrowserTerminalRouteFence{Identity: identity2, Revision: 2, AttachmentGeneration: 2}
	if err := hub.ActivateRoute("route_test", fence2); err != nil {
		t.Fatal(err)
	}
	select {
	case <-publisher.Done():
	case <-ctx.Done():
		t.Fatal("old publisher was not fenced")
	}
	hub.DeactivateRoute("route_test", fence)
	key2 := BrowserTerminalSessionKey{RouteID: "route_test", TerminalSessionID: "term_2", ProcessGeneration: identity2.ProcessGeneration}
	if _, err := hub.Subscribe(key2, fence2, "attach_new"); err != nil {
		t.Fatalf("old detach evicted new generation: %v", err)
	}
}

func TestBrowserTerminalHubBuffersBurstButBoundsSlowViewerBytes(t *testing.T) {
	hub := NewBrowserTerminalHub()
	defer hub.Close()
	identity := testEdgePreviewIdentity(1, 1)
	fence := BrowserTerminalRouteFence{Identity: identity, Revision: 1, AttachmentGeneration: 1}
	if err := hub.ActivateRoute("route_test", fence); err != nil {
		t.Fatal(err)
	}
	key := BrowserTerminalSessionKey{RouteID: "route_test", TerminalSessionID: "term_1", ProcessGeneration: identity.ProcessGeneration}
	viewer, err := hub.Subscribe(key, fence, "attach_viewer")
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := hub.RegisterPublisher(key, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	for i := 0; i < 80; i++ {
		if err := publisher.Publish([]byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < 80; i++ {
		got, err := viewer.Next(ctx)
		if err != nil || len(got) != 1 || got[0] != byte(i) {
			t.Fatalf("burst record %d: %x, %v", i, got, err)
		}
	}
	large := bytes.Repeat([]byte{0xA5}, browserTerminalMaxRecordBytes)
	for i := 0; i <= browserTerminalQueuedBytes/len(large); i++ {
		if err := publisher.Publish(large); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-viewer.Done():
	case <-ctx.Done():
		t.Fatal("slow viewer exceeded byte budget without disconnect")
	}
}
