package config

import (
	"strings"
	"testing"
	"time"
)

func TestForwardKeysDefaultAndValidate(t *testing.T) {
	deployment, err := LoadDeployment(writeDeployment(t, validDeploymentJSON()))
	if err != nil {
		t.Fatal(err)
	}
	if !deployment.ForwardOn() || deployment.ForwardDial() != 3*time.Second || deployment.ForwardOpen() != 3*time.Second ||
		deployment.ForwardPeers() != 10*time.Second || deployment.ForwardSeed() != 5*time.Minute || deployment.ForwardMaxStreams() != 4096 {
		t.Fatalf("defaults = %v %v %v %v %v %v", deployment.ForwardOn(), deployment.ForwardDial(), deployment.ForwardOpen(), deployment.ForwardPeers(), deployment.ForwardSeed(), deployment.ForwardMaxStreams())
	}
	explicit := strings.TrimSuffix(validDeploymentJSON(), "}") + `,"forward_enabled":false,"forward_dial_timeout":1000000000,"forward_open_timeout":2000000000,"forward_peer_interval":5000000000,"forward_seed_interval":60000000000,"forward_max_streams_per_peer":8}`
	if deployment, err = LoadDeployment(writeDeployment(t, explicit)); err != nil {
		t.Fatal(err)
	}
	if deployment.ForwardOn() || deployment.ForwardDial() != time.Second || deployment.ForwardOpen() != 2*time.Second ||
		deployment.ForwardPeers() != 5*time.Second || deployment.ForwardSeed() != time.Minute || deployment.ForwardMaxStreams() != 8 {
		t.Fatalf("explicit = %v %v %v %v %v %v", deployment.ForwardOn(), deployment.ForwardDial(), deployment.ForwardOpen(), deployment.ForwardPeers(), deployment.ForwardSeed(), deployment.ForwardMaxStreams())
	}
	for _, bad := range []string{
		`,"forward_dial_timeout":-1}`, `,"forward_dial_timeout":1000}`, `,"forward_dial_timeout":60000000000}`,
		`,"forward_open_timeout":-1}`, `,"forward_open_timeout":1000}`, `,"forward_open_timeout":60000000000}`,
		`,"forward_peer_interval":-1}`, `,"forward_peer_interval":1000}`, `,"forward_peer_interval":3600000000000}`,
		`,"forward_seed_interval":-1}`, `,"forward_seed_interval":1000}`, `,"forward_seed_interval":86400000000000}`,
		`,"forward_max_streams_per_peer":-1}`, `,"forward_max_streams_per_peer":2000000}`,
	} {
		if _, err := LoadDeployment(writeDeployment(t, strings.TrimSuffix(validDeploymentJSON(), "}")+bad)); err == nil {
			t.Fatalf("accepted forward setting %s", bad)
		}
	}
}

func TestForwardPeerStaleAfterDefaultsAndValidates(t *testing.T) {
	deployment, err := LoadDeployment(writeDeployment(t, validDeploymentJSON()))
	if err != nil {
		t.Fatal(err)
	}
	if deployment.ForwardPeerStale() != 50*time.Second {
		t.Fatalf("default stale = %v, want 50s", deployment.ForwardPeerStale())
	}
	// A longer peer interval scales the default, so the stale window never shrinks below one refresh.
	longer, err := LoadDeployment(writeDeployment(t, strings.TrimSuffix(validDeploymentJSON(), "}")+`,"forward_peer_interval":60000000000}`))
	if err != nil {
		t.Fatal(err)
	}
	if longer.ForwardPeerStale() != 5*time.Minute {
		t.Fatalf("stale with 1m interval = %v, want 5m", longer.ForwardPeerStale())
	}
	explicit, err := LoadDeployment(writeDeployment(t, strings.TrimSuffix(validDeploymentJSON(), "}")+`,"forward_peer_stale_after":30000000000}`))
	if err != nil {
		t.Fatal(err)
	}
	if explicit.ForwardPeerStale() != 30*time.Second {
		t.Fatalf("explicit stale = %v, want 30s", explicit.ForwardPeerStale())
	}
	for _, bad := range []string{
		`,"forward_peer_stale_after":-1}`, `,"forward_peer_stale_after":5000000000}`,
		`,"forward_peer_stale_after":3600000000001}`,
		`,"forward_peer_interval":20000000000,"forward_peer_stale_after":10000000000}`,
	} {
		if _, err := LoadDeployment(writeDeployment(t, strings.TrimSuffix(validDeploymentJSON(), "}")+bad)); err == nil {
			t.Fatalf("accepted forward setting %s", bad)
		}
	}
}
