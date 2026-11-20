package nodemesh

import (
	"fmt"
	"slices"
	"strings"
)

// PeerSet is an immutable peer directory: lookups by node id and by certificate pin.
type PeerSet struct {
	list  []Peer
	byID  map[string]Peer
	byPin map[string]Peer
}

// NewPeerSet validates peers. Duplicate node ids or pins, and more than MaxPeers, are refused whole: a wrong
// directory must fail loudly, and the caller keeps the last good set.
func NewPeerSet(peers []Peer) (PeerSet, error) {
	if len(peers) > MaxPeers {
		return PeerSet{}, fmt.Errorf("%w: more than %d peers", ErrInvalid, MaxPeers)
	}
	s := PeerSet{list: slices.Clone(peers), byID: make(map[string]Peer, len(peers)), byPin: make(map[string]Peer, len(peers))}
	for _, p := range peers {
		if err := p.Validate(); err != nil {
			return PeerSet{}, err
		}
		p.SPKISHA256 = strings.ToLower(p.SPKISHA256)
		if _, dup := s.byID[p.NodeID]; dup {
			return PeerSet{}, fmt.Errorf("%w: duplicate node %q", ErrInvalid, p.NodeID)
		}
		if _, dup := s.byPin[p.SPKISHA256]; dup {
			return PeerSet{}, fmt.Errorf("%w: duplicate pin", ErrInvalid)
		}
		s.byID[p.NodeID], s.byPin[p.SPKISHA256] = p, p
	}
	slices.SortFunc(s.list, func(a, b Peer) int { return strings.Compare(a.NodeID, b.NodeID) })
	return s, nil
}

func (s PeerSet) Lookup(nodeID string) (Peer, bool) { p, ok := s.byID[nodeID]; return p, ok }
func (s PeerSet) ByPin(pin string) (Peer, bool)     { p, ok := s.byPin[strings.ToLower(pin)]; return p, ok }
func (s PeerSet) Len() int                          { return len(s.list) }

// Peers returns the peers ordered by node id.
func (s PeerSet) Peers() []Peer { return slices.Clone(s.list) }
