package tailnet

import (
	"context"
	"sort"
	"time"
)

func (c *CarrierNetwork) RelayNodeIDs() []string {
	c.authority.relay.mu.Lock()
	defer c.authority.relay.mu.Unlock()
	var ids []string
	for _, node := range c.authority.relay.nodes {
		grant := c.authority.relay.grants[node.NodeID]
		if grant.ExpiresAt > time.Now().Unix() && grant.ProcessEpoch == node.ProcessEpoch && grant.NodeGeneration == node.NodeGeneration {
			ids = append(ids, node.NodeID)
		}
	}
	sort.Strings(ids)
	return ids
}

// The signed target offer and our cached grants determine the common relay.
// The old connection's selection is a preference, never extra authority.
func (c *CarrierNetwork) CommonRelay(remote []string, preferred string) string {
	own := c.RelayNodeIDs()
	for _, id := range own {
		for _, other := range remote {
			if id == preferred && id == other {
				return id
			}
		}
	}
	for _, id := range own {
		for _, other := range remote {
			if id == other {
				return id
			}
		}
	}
	return ""
}

// Select per peer, retaining all other peers' active relay routes. Candidate
// signalling over the old connection lets both sides connect to this same
// authorized node without a control-plane request or peer fan-out.
func (c *CarrierNetwork) PreparePeerRelay(ctx context.Context, endpointID, nodeID string) error {
	if nodeID == "" {
		return nil
	}
	allowed := false
	for _, id := range c.RelayNodeIDs() {
		allowed = allowed || id == nodeID
	}
	if !allowed {
		return ErrRegionalAuthority
	}
	binding, err := c.authority.Peer(endpointID)
	if err != nil {
		return err
	}
	peer, err := publicKey(binding.WireGuardPublicKey)
	if err != nil {
		return err
	}
	id := regionalID(nodeID)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrAuthority
	}
	err = c.server.SetPeerRelayRegion(peer, id)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return c.server.PrepareRelay(ctx, id)
}
