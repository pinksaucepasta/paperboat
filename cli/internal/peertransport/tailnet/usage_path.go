package tailnet

import (
	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"time"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=peer-path-accounting
	"tailscale.com/types/key"
)

type UsagePath = mesh.UsagePath

func (a *Authority) PeerUsagePath(endpointID string) (UsagePath, error) {
	return a.PeerUsagePathDirection(endpointID, "")
}

// PeerUsagePathDirection classifies application bytes by observed selected path.
// Upload is traffic received by the host; download is traffic sent by the host.
// An empty direction requires matching recent observations in both directions.
func (a *Authority) PeerUsagePathDirection(endpointID, direction string) (UsagePath, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.usableLocked() {
		return UsagePath{Mode: "unknown"}, ErrAuthority
	}
	for _, peer := range a.current.Peers {
		if peer.Identity.EndpointID != endpointID {
			continue
		}
		public, err := publicKey(peer.Identity.WireGuardPublicKey)
		if err != nil {
			return UsagePath{Mode: "unknown"}, ErrAuthority
		}
		path := UsagePath{Mode: "unknown"}
		if a.clientEngine != nil {
			path = a.clientEngine.PeerUsagePath(public)
		} else if a.server != nil && a.server.server != nil {
			path = a.server.server.PeerUsagePath(public)
		}
		if path.IsRegionalRelay() {
			path.NodeID = a.relay.usageNode(public, direction, time.Now())
		}
		return path, nil
	}
	return UsagePath{Mode: "unknown"}, ErrAdmission
}

func (r *relayAuthority) usageNode(peer key.NodePublic, direction string, now time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	observation := r.usagePeers[peer]
	if observation == nil {
		return ""
	}
	var seen relayUsageSample
	switch direction {
	case "upload":
		seen = observation.receive
	case "download":
		seen = observation.send
	case "":
		if observation.send.node.NodeID != observation.receive.node.NodeID || observation.send.node.NodeGeneration != observation.receive.node.NodeGeneration || observation.send.node.ProcessEpoch != observation.receive.node.ProcessEpoch || now.Sub(observation.send.at) > relayUsageFreshness || now.Sub(observation.receive.at) > relayUsageFreshness {
			return ""
		}
		seen = observation.send
	default:
		return ""
	}
	if seen.at.IsZero() || now.Sub(seen.at) > relayUsageFreshness || seen.at.After(now) {
		return ""
	}
	current := r.nodes[regionalID(seen.node.NodeID)]
	grant, found := r.grants[seen.node.NodeID]
	if current.NodeID != seen.node.NodeID || current.NodeGeneration != seen.node.NodeGeneration || current.ProcessEpoch != seen.node.ProcessEpoch || !found || grant.NodeGeneration != seen.node.NodeGeneration || grant.ProcessEpoch != seen.node.ProcessEpoch || grant.ExpiresAt <= now.Unix() {
		return ""
	}
	return seen.node.NodeID
}
