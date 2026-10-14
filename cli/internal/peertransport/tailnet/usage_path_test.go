package tailnet

import (
	"errors"
	"testing"
)

func TestPeerUsagePathAuthorization(t *testing.T) {
	a, configuration, _, _ := networkTestAuthority(t)
	if _, err := a.PeerUsagePath("machine_test"); !errors.Is(err, ErrAuthority) {
		t.Fatalf("unconfigured authority error = %v", err)
	}
	a.mu.Lock()
	a.current = &configuration
	a.mu.Unlock()
	path, err := a.PeerUsagePath("machine_test")
	if err != nil || path.Mode != "unknown" || path.NodeID != "" {
		t.Fatalf("idle admitted peer = %#v, %v", path, err)
	}
	if _, err := a.PeerUsagePath("another-machine"); !errors.Is(err, ErrAdmission) {
		t.Fatalf("foreign peer error = %v", err)
	}
	a.mu.Lock()
	configuration.ExpiresAt = 1
	a.mu.Unlock()
	if _, err := a.PeerUsagePath("machine_test"); !errors.Is(err, ErrAuthority) {
		t.Fatalf("expired authority error = %v", err)
	}
}
