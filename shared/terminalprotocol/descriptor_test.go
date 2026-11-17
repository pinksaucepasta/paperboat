package terminalprotocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDescriptorUsesCanonicalNames(t *testing.T) {
	expires := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	d := Descriptor{
		Schema: DescriptorSchemaV1, Issuer: "https://api.paperboat.test", Connectable: true, ExpiresAt: expires,
		Environment:  Environment{ID: "env_1", Kind: EnvironmentMachine, ResourceID: "machine_1", Alias: "demo", State: "ready", Root: "/workspace"},
		Capabilities: []string{CapabilityTerminal, CapabilityFileTransfer},
		Terminal:     &Terminal{Protocol: TerminalSubprotocol, Endpoints: TerminalEndpoints{QUIC: "quic://edge.paperboat.test:443", WSS: "wss://edge.paperboat.test/v1/runtime"}, SessionID: "session_1", CWD: "/workspace"},
		FileTransfer: &FileTransfer{Endpoint: "https://edge.paperboat.test/v1/file-transfers", Auth: Auth{Method: "bearer", Token: "token", ExpiresAt: expires, Scopes: []string{"file:transfer"}}, Policy: FileTransferPolicy{Revision: "file-transfer-v1", MaxBatchFiles: 10, AutomaticMaxBatchBytes: 500 << 20, MaxConcurrentTransfers: 2, RetentionSeconds: 604800, DeliveryTimeoutSeconds: 600, MaxPendingSpoolBytes: 1 << 30}},
		Status:       "ready", Reason: "ready",
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, `"alias":"demo"`) || !strings.Contains(got, `"file_transfer"`) || !strings.Contains(got, `"max_pending_spool_bytes":1073741824`) || !strings.Contains(got, `"retry_after_seconds":0`) {
		t.Fatalf("descriptor omits members: %s", got)
	}
	for _, forbidden := range []string{"display_name", "helper", "provider_route", "websocket_base_url", "http_base_url", "project_id", "user_machine_id", "\"machine_id\":", "machine_state"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("descriptor contains legacy name %q: %s", forbidden, got)
		}
	}
}

// A client that runs an exec over its native peer connection alone gets no edge endpoints at all.
func TestOperationWithoutEdgeEndpointsHasAnEmptyEndpointsObject(t *testing.T) {
	raw, err := json.Marshal(Operation{OperationID: "op_1", Environment: Environment{ID: "e", Kind: EnvironmentMachine, ResourceID: "m", Alias: "a", State: "ready", Root: "/w"}, Auth: Auth{Method: "bearer", Token: "t", Scopes: []string{"exec:operate"}}})
	if err != nil || !strings.Contains(string(raw), `"endpoints":{}`) {
		t.Fatalf("operation = %s, %v", raw, err)
	}
}

func TestPolicyBounds(t *testing.T) {
	p := FileTransferPolicy{AutomaticMaxBatchBytes: AutomaticTransferLimit, MaxPendingSpoolBytes: AutomaticTransferLimit}
	if file, batch := p.Bounds(); !p.ValidBounds() || file != batch {
		t.Fatalf("bounds %d/%d", file, batch)
	}
	if (FileTransferPolicy{AutomaticMaxBatchBytes: AutomaticTransferLimit + 1, MaxPendingSpoolBytes: AutomaticTransferLimit + 1}).ValidBounds() {
		t.Fatal("a bound over the automatic limit is valid")
	}
}
