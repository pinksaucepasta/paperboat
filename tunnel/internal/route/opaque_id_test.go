package route

import "testing"

func TestTelemetryAdapterPreservesOpaqueIdentity(t *testing.T) {
	for _, id := range []string{"5b8a43c1-574f-4c56-94e1-a1adf6fc2c83", "rte_existing_01", "pvc_rte_0123456789abcdef", "asn_0123456789abcdef"} {
		if got := allowedTelemetryID(id); got != id {
			t.Errorf("identity dropped: %q -> %q", id, got)
		}
	}
	for _, id := range []string{"customer.example.com", "Bearer_secret", "pbce_abcdefghijklmnop", "pbnative_abcdefghijklmnop", "PBCE_abcdefghijklmnop", "token_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83", "ghp_abcdefghijklmnopqrstuvwxyz", "route\nforged"} {
		if got := allowedTelemetryID(id); got != "" {
			t.Errorf("unsafe identity retained: %q", got)
		}
	}
}
