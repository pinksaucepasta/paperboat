package forward

import "github.com/pinksaucepasta/paperboat/shared/nodemesh"

// SetPeersQuietly replaces the peer directory without closing any link: the state a node is in when a directory
// refresh lands between a link's handshake and a later Open.
func (m *Mesh) SetPeersQuietly(peers []nodemesh.Peer) {
	set, _ := nodemesh.NewPeerSet(peers)
	m.peers.Store(&set)
}
