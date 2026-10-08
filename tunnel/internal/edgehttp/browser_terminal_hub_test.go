package edgehttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	yamux "github.com/libp2p/go-yamux/v5"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	"io"
	"strconv"
	"syscall"
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

func TestBrowserTerminalPublisherFailureAndRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	identity := testEdgePreviewIdentity(1, 1)
	server, client := testEdgePreviewCarrierPair(t, identity)
	hub := NewBrowserTerminalHub()
	defer hub.Close()
	failures := make(chan error, 2)
	hub.OnFailure = func(_ context.Context, err error) { failures <- err }
	fence := BrowserTerminalRouteFence{Identity: identity, Revision: 1, AttachmentGeneration: 1}
	if err := hub.ActivateRoute("route_publisher", fence); err != nil {
		t.Fatal(err)
	}
	key := BrowserTerminalSessionKey{RouteID: "route_publisher", TerminalSessionID: "term_publisher", ProcessGeneration: identity.ProcessGeneration}
	runDone := make(chan error, 1)
	go func() { runDone <- hub.ServeCarrier(ctx, server) }()
	open := func() *datacarrier.Stream {
		stream, err := client.OpenStream(ctx, connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: identity.AccountID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: identity.ProcessGeneration, Generation: identity.Generation, RouteID: key.RouteID, RequestID: key.TerminalSessionID, Kind: datacarrier.BrowserTerminalOutputStream})
		if err != nil {
			t.Fatal(err)
		}
		return stream
	}
	old, err := hub.Subscribe(key, fence, "browser_old")
	if err != nil {
		t.Fatal(err)
	}
	failed := open()
	if _, err := failed.Write([]byte{0, 0, 0, 4, 'x'}); err != nil {
		t.Fatal(err)
	}
	failed.Close()
	select {
	case err := <-failures:
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("lost truncated record cause: %T", err)
		}
	case <-time.After(time.Second):
		t.Fatal("missing publisher failure")
	}
	select {
	case <-old.Done():
	case <-time.After(time.Second):
		t.Fatal("old publisher did not release session")
	}
	current, err := hub.Subscribe(key, fence, "browser_current")
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	recovered := open()
	if _, err := recovered.Write(append([]byte{0, 0, 0, 6}, []byte("opaque")...)); err != nil {
		t.Fatal(err)
	}
	readCtx, stopRead := context.WithTimeout(ctx, time.Second)
	defer stopRead()
	got, err := current.Next(readCtx)
	if err != nil || string(got) != "opaque" {
		t.Fatal("publisher did not recover")
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("publisher workers did not join")
	}
	select {
	case err := <-failures:
		var types []string
		requestCauseMatches(err, func(node error) bool { types = append(types, fmt.Sprintf("%T", node)); return false })
		t.Fatalf("normal shutdown emitted failure: %v EOF=%v truncated=%v", types, errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF))
	default:
	}
}

func TestBrowserTerminalCanceledPublisherRetainsAbnormalReset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var failures []error
	hub := NewBrowserTerminalHub()
	defer hub.Close()
	hub.OnFailure = func(_ context.Context, err error) { failures = append(failures, err) }
	normal := &yamux.StreamError{Remote: true, ErrorCode: 0}
	abnormal := &yamux.StreamError{Remote: true, ErrorCode: 7}
	hub.observe(ctx, normal)
	hub.observe(ctx, abnormal)
	hub.observe(ctx, errors.Join(normal, syscall.EIO))
	if len(failures) != 2 || !errors.Is(failures[0], abnormal) || !errors.Is(failures[1], syscall.EIO) {
		t.Fatal("canceled publisher masked operational reset")
	}
}
