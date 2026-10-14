package previewattachment

import (
	"context"
	"errors"
	"testing"
	"time"
)

type authorityLeaseResolver struct {
	lease LeaseSnapshot
	last  LeaseLookupRequest
}

func (r *authorityLeaseResolver) ResolvePreviewLease(_ context.Context, request LeaseLookupRequest) (LeaseSnapshot, error) {
	r.last = request
	return r.lease, nil
}

type authorityCarrierAllocator struct {
	allocation PreviewCarrierAllocation
	last       PreviewCarrierAllocationRequest
}

func (a *authorityCarrierAllocator) AllocatePreviewCarrier(_ context.Context, request PreviewCarrierAllocationRequest) (PreviewCarrierAllocation, error) {
	a.last = request
	return a.allocation, nil
}

func TestServerAuthorityBindsProofToDurableLeaseAndPreviewEphemeralAllocator(t *testing.T) {
	resolution := testResolution(testNow())
	lease := &authorityLeaseResolver{lease: resolution.Lease}
	allocator := &authorityCarrierAllocator{allocation: PreviewCarrierAllocation{Carrier: resolution.Carrier, Route: resolution.Route}}
	authority := ServerAuthority{Leases: lease, Carriers: allocator}

	result, err := authority.ResolvePreviewAttachment(context.Background(), ResolveRequest{Proof: testProof(), Request: testRequest()})
	if err != nil {
		t.Fatal(err)
	}
	if result.Lease.AccountID != "account-1" || !result.Carrier.Ephemeral || result.Route.RouteID != "route-1" {
		t.Fatalf("resolution = %#v, want account-scoped preview-ephemeral carrier", result)
	}
	if lease.last.UserID != "user-1" || lease.last.MachineID != "machine-1" || lease.last.OwnerSessionID != "owner-session-1" || lease.last.OperationID != "operation-1" {
		t.Fatalf("lease lookup = %#v, want proof/body binding", lease.last)
	}
	if allocator.last.RequestHash == "" || allocator.last.Lease.PreviewID != "preview-1" {
		t.Fatalf("carrier allocation request = %#v", allocator.last)
	}
}

func TestServerAuthorityRejectsOwnerSessionOrOperationMismatchBeforeAllocation(t *testing.T) {
	resolution := testResolution(testNow())
	lease := &authorityLeaseResolver{lease: resolution.Lease}
	allocator := &authorityCarrierAllocator{allocation: PreviewCarrierAllocation{Carrier: resolution.Carrier, Route: resolution.Route}}
	authority := ServerAuthority{Leases: lease, Carriers: allocator}
	req := testRequest()
	req.OwnerSessionID = "owner-session-other"
	if _, err := authority.ResolvePreviewAttachment(context.Background(), ResolveRequest{Proof: testProof(), Request: req}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("owner session mismatch error = %v, want ErrUnauthorized", err)
	}
	if allocator.last.RequestHash != "" {
		t.Fatal("allocator was called before authoritative lease rejected owner session")
	}
}

func TestPreviewCarrierConfigIdentitySharesWorkerAcrossPreviews(t *testing.T) {
	first := PreviewCarrierAllocationRequest{Proof: testProof(), Request: testRequest(), Lease: testResolution(testNow()).Lease}
	session := "session_01234567-89ab-4cde-8fab-0123456789ab"
	second := first
	second.Lease.PreviewID, second.Lease.OperationID = "preview_2", "operation_2"
	firstHash := previewCarrierConfigHash(first, "edge_1", session, "epoch_1")
	if previewCarrierConfigHash(second, "edge_1", session, "epoch_2") != firstHash {
		t.Fatal("same worker preview/edge renewal changed carrier configuration")
	}
	if previewCarrierConfigHash(first, "edge_1", "session_11234567-89ab-4cde-8fab-0123456789ab") == firstHash {
		t.Fatal("new worker session reused carrier configuration identity")
	}
	second.Proof.InstallationGeneration++
	if previewCarrierConfigHash(second, "edge_1", session) == firstHash {
		t.Fatal("new installation reused carrier configuration identity")
	}
}

func testNow() time.Time {
	return time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
}
