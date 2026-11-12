package usermachines

import "testing"

// The managed ingress is the bare public host; the self-hosted one adds the edge-only /v1/runtime/<host> prefix.
// Both must give a native client the receiver's own path.
func TestNativeFileTransferEndpointHasNoEdgePrefix(t *testing.T) {
	for ingress, want := range map[string]string{
		"https://machine.example.test":                                   "https://machine.example.test/v1/file-transfers",
		"https://edge.example.test/v1/runtime/machine.example.test":      "https://edge.example.test/v1/file-transfers",
		"https://edge.example.test:8443/v1/runtime/machine.example.test": "https://edge.example.test:8443/v1/file-transfers",
	} {
		if got := nativeFileTransferEndpoint(ingress); got != want {
			t.Errorf("%s -> %s, want %s", ingress, got, want)
		}
	}
}
