package preview

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents/eventstest"
)

func TestAttachmentCarrierMovesStillConnectedPreviewOnPlacementChange(t *testing.T) {
	now := time.Now().UTC()
	identity := testPreviewCarrierIdentity(1)
	lease, attachment := providerTestLeaseAttachment(t, now, "preview_move", "operation_move_01", "route_move_01", identity, 1)
	var calls atomic.Int32
	allocator := attachmentAllocatorFunc(func(context.Context, AttachmentRequest) (Attachment, error) {
		if calls.Add(1) >= 2 {
			moved := attachment
			moved.Binding.EdgeNodeID = "edge_other"
			moved.AttachmentGeneration++
			return moved, nil
		}
		return attachment, nil
	})
	var stopped atomic.Bool
	provider := &carrierProviderFunc{newCarrier: func() Carrier {
		return &sessionCarrier{run: func(ctx context.Context, observed Lease, ready func(Lease) error) error {
			if err := ready(observed); err != nil {
				return err
			}
			<-ctx.Done()
			stopped.Store(true)
			return ctx.Err()
		}}
	}}
	carrier, err := NewAttachmentCarrier(AttachmentCarrierConfig{
		Attachments: allocator, Provider: provider, PlacementPollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = carrier.RunWithLease(ctx, func() Lease { return lease }, nil, func(Lease) error { return nil })
	if !errors.Is(err, ErrAttachmentPlacementChanged) || !stopped.Load() || ctx.Err() != nil {
		t.Fatalf("placement move err=%v stopped=%v context=%v", err, stopped.Load(), ctx.Err())
	}
}

func TestAttachmentCarrierRetainsOperationRequestAcrossRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Now().UTC()
	identity := testPreviewCarrierIdentity(1)
	lease, attachment := providerTestLeaseAttachment(t, now, "preview_lazy", "operation_lazy_01", "route_lazy_01", identity, 1)
	var (
		mu       sync.Mutex
		requests []AttachmentRequest
		attempt  int
	)
	allocator := attachmentAllocatorFunc(func(ctx context.Context, request AttachmentRequest) (Attachment, error) {
		if err := ctx.Err(); err != nil {
			return Attachment{}, err
		}
		mu.Lock()
		requests = append(requests, request)
		attempt++
		current := attempt
		mu.Unlock()
		result := attachment
		result.RequestID = request.RequestID
		result.CorrelationID = request.CorrelationID
		var err error
		result.RequestHash, err = request.Hash(result.AccountID)
		if err != nil {
			return Attachment{}, err
		}
		if current == 0 {
			return Attachment{}, errors.New("unreachable")
		}
		return result, nil
	})
	provider := &carrierProviderFunc{newCarrier: func() Carrier {
		return &sessionCarrier{run: func(context.Context, Lease, func(Lease) error) error {
			mu.Lock()
			current := attempt
			mu.Unlock()
			if current == 1 {
				return &RetryableCarrierError{Err: errors.New("edge disconnected")}
			}
			return nil
		}}
	}}
	requestCalls := 0
	carrier, err := NewAttachmentCarrier(AttachmentCarrierConfig{
		Attachments: allocator, Provider: provider,
		RequestID: func() (string, error) {
			requestCalls++
			return "request_lazy_01", nil
		},
		CorrelationID: func() (string, error) { return "correlation_lazy_01", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	err = carrier.Run(ctx, lease, func(Lease) error { return nil })
	var retryable *RetryableCarrierError
	if !errors.As(err, &retryable) {
		t.Fatalf("first run error = %v, want retryable", err)
	}
	if err := carrier.Run(ctx, lease, func(Lease) error { return nil }); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotRequests := append([]AttachmentRequest(nil), requests...)
	mu.Unlock()
	if len(gotRequests) != 2 || gotRequests[0] != gotRequests[1] {
		t.Fatalf("attachment requests across retry = %#v, want identical operation/body envelope", gotRequests)
	}
	if requestCalls != 1 {
		t.Fatalf("request ID callbacks = %d, want one retained request across retries", requestCalls)
	}
	if err := carrier.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAttachmentCarrierRequiresDurableCreateOperation(t *testing.T) {
	carrier, err := NewAttachmentCarrier(AttachmentCarrierConfig{
		Attachments: attachmentAllocatorFunc(func(context.Context, AttachmentRequest) (Attachment, error) { return Attachment{}, nil }),
		Provider:    carrierProviderFunc{newCarrier: func() Carrier { return &sessionCarrier{} }},
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := Lease{ID: "preview_missing", OwnerMachineID: "machine_01", OwnerSessionID: "owner_session_01"}
	if err := carrier.Run(context.Background(), lease, func(Lease) error { return nil }); !errors.Is(err, ErrAttachmentBinding) {
		t.Fatalf("missing create operation error = %v, want binding error", err)
	}
	if err := carrier.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAttachmentCarrierRetriesStaleLeaseETag(t *testing.T) {
	err := classifyAttachmentCarrierError(errors.Join(ErrAttachmentLeaseETagStale, &AttachmentHTTPError{StatusCode: 412}))
	var retryable *RetryableCarrierError
	if !errors.As(err, &retryable) || !errors.Is(err, ErrAttachmentLeaseETagStale) {
		t.Fatalf("stale ETag error = %v, want retryable stale precondition", err)
	}
}

func TestAttachmentCarrierAdmitsBeforeProviderAndObservesOriginBeforeLeaseReady(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	identity := testPreviewCarrierIdentity(1)
	lease, base := providerTestLeaseAttachment(t, now, "preview_admit", "operation_admit_01", "route_admit_01", identity, 1)
	pending := base
	pending.State, pending.EdgeReady, pending.OriginReady = "pending", false, false
	admitted := pending
	admitted.State, admitted.EdgeReady, admitted.AttachmentGeneration = "admitted", false, 2
	edgeReady := admitted
	edgeReady.State, edgeReady.EdgeReady, edgeReady.AttachmentGeneration = "edge_ready", true, 3
	readyAttachment := edgeReady
	readyAttachment.State, readyAttachment.EdgeReady, readyAttachment.OriginReady = "ready", true, true
	readyAttachment.AttachmentGeneration = 4
	allocator := &recordingAttachmentAllocator{pending: pending, admitted: admitted, edgeReady: edgeReady, ready: readyAttachment}
	carrier, err := NewAttachmentCarrier(AttachmentCarrierConfig{
		Attachments: allocator,
		Provider: carrierProviderFunc{newCarrier: func() Carrier {
			allocator.record("provider")
			return &sessionCarrier{run: func(_ context.Context, _ Lease, ready func(Lease) error) error {
				return ready(sessionReadyLease(lease))
			}}
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	readyCalls := 0
	if err := carrier.Run(ctx, lease, func(Lease) error {
		allocator.record("lease-ready")
		readyCalls++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if readyCalls != 1 {
		t.Fatalf("lease ready calls = %d, want one", readyCalls)
	}
	if got := allocator.originResults(); len(got) != 1 || !got[0] {
		t.Fatalf("origin observations = %#v, want one successful observation", got)
	}
	want := []string{"allocate", "wait-admission", "provider", "wait-edge-ready", "observe-origin", "lease-ready"}
	if got := allocator.events(); !equalStrings(got, want) {
		t.Fatalf("attachment lifecycle = %#v, want %#v", got, want)
	}
}

func TestAttachmentCarrierDoesNotDialWhilePending(t *testing.T) {
	now := time.Now().UTC()
	identity := testPreviewCarrierIdentity(1)
	lease, attachment := providerTestLeaseAttachment(t, now, "preview_wait_edge", "operation_wait_edge_01", "route_wait_edge_01", identity, 1)
	attachment.State = "pending"
	attachment.EdgeReady = false
	attachment.OriginReady = false
	allocator := &recordingAttachmentAllocator{pending: attachment, admitted: attachment}
	providerCalls := 0
	carrier, err := NewAttachmentCarrier(AttachmentCarrierConfig{
		Attachments: allocator,
		Provider: carrierProviderFunc{newCarrier: func() Carrier {
			providerCalls++
			return &sessionCarrier{run: func(context.Context, Lease, func(Lease) error) error { return nil }}
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = carrier.Run(context.Background(), lease, func(Lease) error { return nil })
	if !errors.Is(err, ErrAttachmentAdmissionPending) {
		t.Fatalf("pending admission error = %v, want pending admission", err)
	}
	var retryable *RetryableCarrierError
	if !errors.As(err, &retryable) {
		t.Fatalf("pending admission error = %v, want retryable", err)
	}
	if providerCalls != 0 {
		t.Fatalf("provider calls = %d, want zero before edge readiness", providerCalls)
	}
}

func TestAttachmentCarrierReportsOriginTimeoutAfterEdgeReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	now := time.Now().UTC()
	identity := testPreviewCarrierIdentity(1)
	lease, attachment := providerTestLeaseAttachment(t, now, "preview_origin_timeout", "operation_origin_timeout_01", "route_origin_timeout_01", identity, 1)

	allocator := &recordingAttachmentAllocator{pending: attachment, admitted: attachment, ready: attachment}
	pair := newPreviewCarrierPair(t, ctx, identity)
	defer pair.close()
	hub, err := NewDataCarrierPreviewHub(ctx, DataCarrierPreviewHubConfig{Active: pair.active, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	previewCarrier, err := NewDataCarrierPreviewCarrier(DataCarrierPreviewCarrierConfig{
		Hub: hub, Identity: identity, RouteID: attachment.Binding.RouteID,
		OriginDialTimeout: 10 * time.Millisecond,
		DialOrigin: func(ctx context.Context, _ LeaseTarget) (io.ReadWriteCloser, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	carrier, err := NewAttachmentCarrier(AttachmentCarrierConfig{
		Attachments: allocator,
		Provider:    carrierProviderFunc{newCarrier: func() Carrier { return previewCarrier }},
	})
	if err != nil {
		t.Fatal(err)
	}
	readyCalls := 0
	err = carrier.Run(ctx, lease, func(Lease) error {
		readyCalls++
		return nil
	})
	if !errors.Is(err, ErrDataCarrierPreviewOrigin) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("origin timeout error = %v, want origin and deadline causes", err)
	}
	var retryable *RetryableCarrierError
	if !errors.As(err, &retryable) {
		t.Fatalf("origin timeout error = %v, want retryable carrier error", err)
	}
	if readyCalls != 0 {
		t.Fatalf("ready callback calls = %d, want zero", readyCalls)
	}
	if got := allocator.originResults(); len(got) != 1 || got[0] {
		t.Fatalf("origin observations = %#v, want one false result", got)
	}
	if got := allocator.events(); !equalStrings(got, []string{"allocate", "wait-admission", "observe-origin"}) {
		t.Fatalf("origin timeout lifecycle = %#v, want allocation, admission, then origin observation", got)
	}
	if err := carrier.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type attachmentAllocatorFunc func(context.Context, AttachmentRequest) (Attachment, error)

func (f attachmentAllocatorFunc) Allocate(ctx context.Context, request AttachmentRequest) (Attachment, error) {
	return f(ctx, request)
}

func (f attachmentAllocatorFunc) WaitForAdmission(_ context.Context, _ AttachmentRequest, initial Attachment) (Attachment, error) {
	return initial, nil
}

func (f attachmentAllocatorFunc) ObserveOrigin(_ context.Context, _ AttachmentRequest, attachment Attachment, _ bool) (Attachment, error) {
	return attachment, nil
}

type carrierProviderFunc struct {
	newCarrier func() Carrier
}

func (p carrierProviderFunc) CarrierForAttachment(context.Context, Lease, Attachment) (Carrier, error) {
	if p.newCarrier == nil {
		return nil, ErrPreviewCarrierProviderUnavailable
	}
	return p.newCarrier(), nil
}

func (carrierProviderFunc) Close(context.Context) error { return nil }

var _ AttachmentAllocator = attachmentAllocatorFunc(nil)
var _ AttachmentAdmissionWaiter = attachmentAllocatorFunc(nil)
var _ AttachmentReadinessObserver = attachmentAllocatorFunc(nil)
var _ PreviewCarrierProvider = carrierProviderFunc{}

type recordingAttachmentAllocator struct {
	mu        sync.Mutex
	pending   Attachment
	admitted  Attachment
	edgeReady Attachment
	ready     Attachment
	originV   []bool
	eventsV   []string
}

func (a *recordingAttachmentAllocator) Allocate(context.Context, AttachmentRequest) (Attachment, error) {
	a.record("allocate")
	return a.pending, nil
}

func (a *recordingAttachmentAllocator) WaitForAdmission(context.Context, AttachmentRequest, Attachment) (Attachment, error) {
	a.record("wait-admission")
	return a.admitted, nil
}

func (a *recordingAttachmentAllocator) WaitForEdgeReady(context.Context, AttachmentRequest, Attachment) (Attachment, error) {
	a.record("wait-edge-ready")
	return a.edgeReady, nil
}

func (a *recordingAttachmentAllocator) ObserveOrigin(_ context.Context, _ AttachmentRequest, _ Attachment, originReady bool) (Attachment, error) {
	a.record("observe-origin")
	a.mu.Lock()
	a.originV = append(a.originV, originReady)
	ready := a.ready
	a.mu.Unlock()
	return ready, nil
}

func (a *recordingAttachmentAllocator) record(event string) {
	a.mu.Lock()
	a.eventsV = append(a.eventsV, event)
	a.mu.Unlock()
}

func (a *recordingAttachmentAllocator) events() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.eventsV...)
}

func (a *recordingAttachmentAllocator) originResults() []bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]bool(nil), a.originV...)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// A preview change published by the server moves a still-connected preview at once, without waiting
// for the placement poll.
func TestAttachmentCarrierChecksPlacementWhenTheServerReportsAPreviewChange(t *testing.T) {
	now := time.Now().UTC()
	identity := testPreviewCarrierIdentity(1)
	lease, attachment := providerTestLeaseAttachment(t, now, "preview_event", "operation_event_01", "route_event_01", identity, 1)
	var moved atomic.Bool
	allocator := attachmentAllocatorFunc(func(context.Context, AttachmentRequest) (Attachment, error) {
		if moved.Load() {
			next := attachment
			next.Binding.EdgeNodeID = "edge_other"
			next.AttachmentGeneration++
			return next, nil
		}
		return attachment, nil
	})
	provider := &carrierProviderFunc{newCarrier: func() Carrier {
		return &sessionCarrier{run: func(ctx context.Context, observed Lease, ready func(Lease) error) error {
			if err := ready(observed); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		}}
	}}
	_, subscriptions, plane := eventstest.Start(t, time.Hour, machineevents.TopicPreviews)
	carrier, err := NewAttachmentCarrier(AttachmentCarrierConfig{
		Attachments: allocator, Provider: provider, PlacementPollInterval: time.Minute, PlacementEvents: subscriptions[0],
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- carrier.RunWithLease(ctx, func() Lease { return lease }, nil, func(Lease) error { close(ready); return nil })
	}()
	<-ready
	moved.Store(true)
	plane.Publish(machineevents.TopicPreviews)
	select {
	case err := <-result:
		if !errors.Is(err, ErrAttachmentPlacementChanged) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the preview change was not noticed before the placement poll")
	}
}

// The server binds an attachment to the hash of its request, trace IDs included. A carrier created
// after a host runtime restart must therefore send the same request as the one that allocated it, or
// the server refuses the re-attach (409) and the preview ends.
func TestAttachmentRequestIsTheSameForACarrierCreatedAfterARestart(t *testing.T) {
	now := time.Date(2098, 1, 2, 3, 4, 5, 0, time.UTC)
	identity := testPreviewCarrierIdentity(1)
	lease, _ := providerTestLeaseAttachment(t, now, "preview_restart", "operation_restart_01", "route_restart_01", identity, 1)
	var requests []AttachmentRequest
	for range 2 {
		carrier, err := NewAttachmentCarrier(AttachmentCarrierConfig{
			Attachments: attachmentAllocatorFunc(func(context.Context, AttachmentRequest) (Attachment, error) { return Attachment{}, errors.New("not called") }),
			Provider:    &carrierProviderFunc{newCarrier: func() Carrier { return &sessionCarrier{} }},
		})
		if err != nil {
			t.Fatal(err)
		}
		request, err := carrier.requestForLease(lease)
		if err != nil {
			t.Fatal(err)
		}
		requests = append(requests, request)
	}
	first, second := requests[0], requests[1]
	if first.RequestID != second.RequestID || first.CorrelationID != second.CorrelationID || first.IdempotencyKey != second.IdempotencyKey {
		t.Fatalf("a restarted carrier sent a different request: %+v vs %+v", first, second)
	}
}
