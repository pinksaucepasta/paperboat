package previewdispatch

import (
	"testing"
	"time"
)

// GoldenRequest is the cross-module vector: the cli and the server each rebuild it with their own fixture types
// and hash it through their own validation path; all three must get GoldenHash, which was computed from the
// JSON spelled out independently of Go's encoder.
func GoldenRequest() Request {
	deadline := time.Date(2030, 1, 1, 0, 30, 0, 0, time.UTC)
	return Request{
		Lazy:   &LazyBinding{PolicyID: "pol_1", PolicyGeneration: 2, InstallationGeneration: 3, BootID: "boot_0123456789abcdef"},
		Schema: "paperboat.preview-tunnel/v1", Kind: "preview_dispatch", PreviewID: "prv_1", OperationID: "op_1", AccountID: "acct_1", ActorID: "actor_1",
		OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "lazy_runtime",
		Target:     Target{Scheme: "http", Address: "127.0.0.1:3000", HostHeader: "app.local"},
		AccessMode: "private", Endpoint: "https://app.example.test", LeaseDeadline: time.Date(2030, 1, 1, 1, 0, 0, 0, time.UTC), UserDeadline: &deadline,
		LeaseETag: `"ptv1:preview_lease:cHJ2XzE:7"`, State: "connecting", AllocationState: "ready", EdgeState: "ready", OriginState: "unknown",
		CreatedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), LastRenewedAt: time.Date(2030, 1, 1, 0, 10, 0, 0, time.UTC), ExpectedGeneration: 7,
		IdempotencyKey: "idem_1", RequestID: "req_1", CorrelationID: "cor_1", RequestHash: "ignored",
	}
}

const GoldenHash = "e142084de5fe437e40d9db15cff56f406aed2a4af2c98d314a658b3d3769e1dc"

func TestHashInputIsByteStable(t *testing.T) {
	input, err := MarshalHashInput(Normalize(GoldenRequest()))
	if err != nil || HashOf(input) != GoldenHash {
		t.Fatalf("hash = %q, %v", HashOf(input), err)
	}
}

func TestNormalizeTrimsWithoutAliasingLazy(t *testing.T) {
	r := GoldenRequest()
	r.PreviewID, r.Target.Scheme, r.Target.HostHeader, r.Lazy.PolicyID = " prv_1 ", " HTTP ", " App.Local ", " pol_1 "
	n := Normalize(r)
	if n.PreviewID != "prv_1" || n.Target.Scheme != "http" || n.Target.HostHeader != "app.local" || n.Lazy.PolicyID != "pol_1" || r.Lazy.PolicyID != " pol_1 " {
		t.Fatalf("normalized = %#v (input lazy %#v)", n, r.Lazy)
	}
	if input, _ := MarshalHashInput(n); HashOf(input) != GoldenHash {
		t.Fatal("untidy request does not hash like the tidy one")
	}
}
