package usermachines

import "testing"

// The ingress is the machine's bare public host; a native client gets the receiver's own path on it.
func TestNativeFileTransferEndpointIsTheIngressHostWithTheReceiversPath(t *testing.T) {
	for ingress, want := range map[string]string{
		"https://machine.example.test":  "https://machine.example.test/v1/file-transfers",
		"https://machine.example.test/": "https://machine.example.test/v1/file-transfers",
	} {
		if got := nativeFileTransferEndpoint(ingress); got != want {
			t.Errorf("%s -> %s, want %s", ingress, got, want)
		}
	}
}
