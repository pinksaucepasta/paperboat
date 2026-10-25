package tailnet

import (
	"context"
	"time"
)

// PathKind is which kind of path currently carries a peer's traffic, for file transfer modes (PLAN 3b
// "File transfer modes"): Unlimited transfers may use only PathDirect and PathSelfHostedRelay.
type PathKind string

const (
	PathDirect          PathKind = "direct"
	PathSelfHostedRelay PathKind = "self_hosted_relay"
	PathHostedRelay     PathKind = "hosted_relay"
	PathNone            PathKind = "none"
)

// pathEligibilityPoll is how often PathEligibility looks for a change; the engine's path observation
// itself is cached for about a second.
const pathEligibilityPoll = 500 * time.Millisecond

// PathKind classifies the path to the peer endpointID. A regional relay counts as self-hosted only when
// the relay that carries it is known and its signed candidate says so; when the carrying relay cannot be
// proven (a peer relay, or no recent observation), only a relay set made entirely of the account's own
// relays counts. The engine picks one path per peer for every stream, so a transfer cannot route around a
// relay other traffic uses: it can only wait.
func (a *Authority) PathKind(endpointID string) (PathKind, error) {
	path, err := a.PeerUsagePathDirection(endpointID, "")
	if err != nil {
		return PathNone, err
	}
	a.relay.mu.Lock()
	defer a.relay.mu.Unlock()
	return classifyPath(path, a.relay.nodes), nil
}

// classifyPath is PathKind for an observed path and the engine's current relay nodes by region.
func classifyPath[K comparable](path UsagePath, nodes map[K]RegionalNode) PathKind {
	switch path.Mode {
	case "direct":
		return PathDirect
	case "relay":
	default:
		return PathNone
	}
	if path.NodeID != "" {
		for _, node := range nodes {
			if node.NodeID == path.NodeID {
				if node.SelfHosted() {
					return PathSelfHostedRelay
				}
				return PathHostedRelay
			}
		}
		return PathHostedRelay
	}
	if len(nodes) == 0 {
		return PathHostedRelay
	}
	for _, node := range nodes {
		if !node.SelfHosted() {
			return PathHostedRelay
		}
	}
	return PathSelfHostedRelay
}

// PathEligibility reports the current PathKind to endpointID and a channel closed once the kind changes
// or ctx ends, whichever comes first; call it again after the channel closes. An unknown peer is PathNone.
func (a *Authority) PathEligibility(ctx context.Context, endpointID string) (PathKind, <-chan struct{}) {
	kind, _ := a.PathKind(endpointID)
	changed := make(chan struct{})
	go func() {
		defer close(changed)
		ticker := time.NewTicker(pathEligibilityPoll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-a.done:
				return
			case <-ticker.C:
				if now, _ := a.PathKind(endpointID); now != kind {
					return
				}
			}
		}
	}()
	return kind, changed
}
