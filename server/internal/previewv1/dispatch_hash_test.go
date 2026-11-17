package previewv1

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/previewdispatch"
)

// The shared previewdispatch golden vector, rebuilt with this module's own fixture and hashed through the server's
// validation path: the server must sign the dispatch exactly as the machine verifies it
// (shared/previewdispatch TestHashInputIsByteStable and the cli's TestDispatchRequestHashMatchesTheSharedGoldenVector pin the same value).
func TestDispatchRequestHashMatchesTheSharedGoldenVector(t *testing.T) {
	deadline := time.Date(2030, 1, 1, 0, 30, 0, 0, time.UTC)
	request := DispatchRequest{Request: previewdispatch.Request{
		Lazy:   &LazyBinding{PolicyID: "pol_1", PolicyGeneration: 2, InstallationGeneration: 3, BootID: "boot_0123456789abcdef"},
		Schema: Schema, Kind: PreviewDispatchKind, PreviewID: "prv_1", OperationID: "op_1", AccountID: "acct_1", ActorID: "actor_1",
		OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "lazy_runtime",
		Target:     Target{Scheme: "http", Address: "127.0.0.1:3000", HostHeader: "App.Local"},
		AccessMode: "private", Endpoint: "https://app.example.test", LeaseDeadline: time.Date(2030, 1, 1, 1, 0, 0, 0, time.UTC), UserDeadline: &deadline,
		LeaseETag: `"ptv1:preview_lease:cHJ2XzE:7"`, State: "connecting", AllocationState: "ready", EdgeState: "ready", OriginState: "unknown",
		CreatedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), LastRenewedAt: time.Date(2030, 1, 1, 0, 10, 0, 0, time.UTC), ExpectedGeneration: 7,
		IdempotencyKey: "idem_1", RequestID: "req_1", CorrelationID: "cor_1",
	}}
	got, err := request.ComputeRequestHash()
	if err != nil || got != "e142084de5fe437e40d9db15cff56f406aed2a4af2c98d314a658b3d3769e1dc" {
		t.Fatalf("hash = %q, %v", got, err)
	}
}
