package peernetwork

import "testing"

// The signed configuration was capped at 128 KiB while it named up to 2048 peers of ~1.3 KiB each, so
// an account past ~100 endpoints got 409 peer_network_too_large and new carriers exited at start
// (2026-10-22). The byte bound must carry MaxEndpoints peers.
func TestConfigurationBytesCarryEveryEndpoint(t *testing.T) {
	const measuredPeerWithTwoScopes = 1298
	if MaxPeerBytes < measuredPeerWithTwoScopes || MaxConfigurationBytes < MaxEndpoints*MaxPeerBytes {
		t.Fatalf("MaxConfigurationBytes=%d cannot carry %d peers of %d bytes", MaxConfigurationBytes, MaxEndpoints, MaxPeerBytes)
	}
}
