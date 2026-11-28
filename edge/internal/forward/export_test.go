package forward

import "github.com/pinksaucepasta/paperboat/shared/nodemesh"

// SetPeersQuietly replaces the peer directory without closing any link: the state a node is in when a directory
// refresh lands between a link's handshake and a later Open.
func (m *Mesh) SetPeersQuietly(peers []nodemesh.Peer) {
	set, _ := nodemesh.NewPeerSet(peers)
	m.peers.Store(&set)
}

// PeerPin is the pin the applied directory lists for node id.
func (m *Mesh) PeerPin(id string) string {
	p, _ := m.peerSet().Lookup(id)
	return p.SPKISHA256
}

// LiveLink identifies the open link to node id, or nil.
func (m *Mesh) LiveLink(id string) any {
	if l := m.liveLink(id); l != nil {
		return l
	}
	return nil
}
