package tailnet

import (
	"context"
	"net/netip"

	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=delegated-generation-revocation
	"tailscale.com/types/key"
)

// Watch closes authenticated sessions before removing their packet paths, so
// revocation can deliver an application close while the underlay still works.
// A nil list means the generation's own machine identity was withdrawn.
func (c *CarrierNetwork) Watch(ctx context.Context, revoke func([]string)) {
	watch, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
	go func() {
		defer cancel()
		for {
			changed := c.authority.ScopeChanges()
			c.authority.mu.Lock()
			valid := c.authority.usableLocked() && c.authority.current.Self == c.self
			var peers map[key.NodePublic]netip.Addr
			if valid {
				peers, _ = c.authority.peersLocked()
			}
			c.authority.mu.Unlock()
			if !valid {
				revoke(nil)
				c.Close()
				return
			}
			c.mu.Lock()
			bindings := make(map[string]NetworkBinding, len(c.bindings))
			for id, binding := range c.bindings {
				bindings[id] = binding
			}
			c.mu.Unlock()
			var removed []string
			for id, binding := range bindings {
				current, err := c.authority.Peer(id)
				public, keyErr := publicKey(binding.WireGuardPublicKey)
				if err != nil || current != binding || keyErr != nil || !peers[public].IsValid() {
					removed = append(removed, id)
				}
			}
			if len(removed) != 0 {
				revoke(removed)
			}
			c.mu.Lock()
			if c.closed {
				c.mu.Unlock()
				return
			}
			for _, id := range removed {
				public, _ := publicKey(c.bindings[id].WireGuardPublicKey)
				delete(c.contacts, public)
				delete(c.bindings, id)
			}
			err := c.server.ReplaceAllowedPeers(peers)
			if err == nil {
				err = c.server.SetPeerContacts(c.contacts)
			}
			c.mu.Unlock()
			if err != nil {
				revoke(nil)
				c.Close()
				return
			}
			select {
			case <-watch.Done():
				return
			case <-changed:
			}
		}
	}()
}
