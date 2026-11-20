package config

import (
	"errors"
	"time"
)

// Defaults of the node-to-node forward (edge/internal/forward). A forward that cannot be opened in a few seconds
// is better answered by the next holder than waited for.
const (
	DefaultForwardEnabled           = true
	DefaultForwardDialTimeout       = 3 * time.Second
	DefaultForwardOpenTimeout       = 3 * time.Second
	DefaultForwardPeerInterval      = 10 * time.Second
	DefaultForwardSeedInterval      = 5 * time.Minute
	DefaultForwardMaxStreamsPerPeer = 4096
)

// ForwardOn reports whether this node forwards visitors to, and accepts forwards from, other managed nodes.
func (d Deployment) ForwardOn() bool {
	if d.ForwardEnabled == nil {
		return DefaultForwardEnabled
	}
	return *d.ForwardEnabled
}

func (d Deployment) ForwardDial() time.Duration {
	return orDefault(d.ForwardDialTimeout, DefaultForwardDialTimeout)
}

func (d Deployment) ForwardOpen() time.Duration {
	return orDefault(d.ForwardOpenTimeout, DefaultForwardOpenTimeout)
}

func (d Deployment) ForwardPeers() time.Duration {
	return orDefault(d.ForwardPeerInterval, DefaultForwardPeerInterval)
}

func (d Deployment) ForwardSeed() time.Duration {
	return orDefault(d.ForwardSeedInterval, DefaultForwardSeedInterval)
}

func (d Deployment) ForwardMaxStreams() int {
	if d.ForwardMaxStreamsPerPeer == 0 {
		return DefaultForwardMaxStreamsPerPeer
	}
	return d.ForwardMaxStreamsPerPeer
}

func orDefault(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	return value
}

func outside(value, low, high time.Duration) bool { return value != 0 && (value < low || value > high) }

func (d Deployment) validateForward() error {
	switch {
	case outside(d.ForwardDialTimeout, 100*time.Millisecond, 30*time.Second):
		return errors.New("forward_dial_timeout must be between 100ms and 30s")
	case outside(d.ForwardOpenTimeout, 100*time.Millisecond, 30*time.Second):
		return errors.New("forward_open_timeout must be between 100ms and 30s")
	case outside(d.ForwardPeerInterval, time.Second, time.Minute):
		return errors.New("forward_peer_interval must be between 1s and 1m")
	case outside(d.ForwardSeedInterval, 10*time.Second, time.Hour):
		return errors.New("forward_seed_interval must be between 10s and 1h")
	case d.ForwardMaxStreamsPerPeer < 0 || d.ForwardMaxStreamsPerPeer > 1<<20:
		return errors.New("forward_max_streams_per_peer must be between 1 and 1048576")
	}
	return nil
}
