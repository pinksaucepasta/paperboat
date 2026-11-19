package derpquic

import (
	"crypto/sha256"
	"encoding/binary"
	"time"

	"go4.org/mem"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=namespaced-generation-key-derivation
	"tailscale.com/types/key"
)

// A generation separates transport state, never machine identity or authority.
// Both endpoints use the machine-signed carrier certificate's SHA-256 as the
// namespace. Grants, pair permissions, revocation fences and quotas stay owned
// by the existing root identity. The primary carrier uses the zero namespace.
func routeKey(root key.NodePublic, namespace [32]byte) key.NodePublic {
	if namespace == ([32]byte{}) {
		return root
	}
	digest := sha256.Sum256(append(root.AppendTo(nil), namespace[:]...))
	return key.NodePublicFromRaw32(mem.B(digest[:]))
}

func authFrame(token []byte, namespace [32]byte, disco key.DiscoPublic) (byte, []byte) {
	if namespace == ([32]byte{}) {
		return frameAuth, token
	}
	body := append(append(namespace[:], disco.AppendTo(nil)...), 0, 0, 0, 1)
	return frameAuthGeneration, append(body, token...)
}

func (s *Server) authenticate(p *peerConn, kind byte, body []byte) error {
	var namespace [32]byte
	var disco key.DiscoPublic
	count := uint32(1)
	switch kind {
	case frameAuth:
	case frameAuthBatch:
		if len(body) <= 4 {
			return ErrProtocol
		}
		count = binary.BigEndian.Uint32(body[:4])
		body = body[4:]
	case frameAuthGeneration:
		if len(body) <= 68 {
			return ErrProtocol
		}
		copy(namespace[:], body[:32])
		disco = key.DiscoPublicFromRaw32(mem.B(body[32:64]))
		count = binary.BigEndian.Uint32(body[64:68])
		body = body[68:]
		if namespace == ([32]byte{}) || disco.IsZero() {
			return ErrProtocol
		}
	default:
		return ErrProtocol
	}
	if count == 0 || count > MaxGrantParts {
		return ErrProtocol
	}
	if !p.key.IsZero() && (p.namespace != namespace || p.generationDisco != disco) {
		return ErrAdmission
	}
	if p.key.IsZero() {
		p.namespace = namespace
		p.generationDisco = disco
	}
	return s.admitAuthParts(p, body, count, time.Now())
}

// liveGrantKey names a connected peer by its key and endpoint identity, the pair liveGrantLocked matches on.
type liveGrantKey struct {
	key key.NodePublic
	id  string
}

// liveGrantsLocked indexes every connected peer once. A pass over all fences (AuthoritySubjects every 3 s, the fence sweep) asked
// liveGrantLocked per fence, which walks every peer: N fences x N peers under the server lock, which stalled accepts from ~6,000
// connections (QUIC CONNECTION_REFUSED while the relay used 35% CPU).
func (s *Server) liveGrantsLocked() map[liveGrantKey]struct{} {
	live := make(map[liveGrantKey]struct{}, len(s.peers))
	for _, peer := range s.peers {
		live[liveGrantKey{peer.key, grantID(peer.grant)}] = struct{}{}
	}
	return live
}

func liveIn(live map[liveGrantKey]struct{}, g Grant) bool {
	k, err := ParseKey(g.WireGuardPublicKey)
	if err != nil {
		return false
	}
	_, ok := live[liveGrantKey{k, grantID(g)}]
	return ok
}

func (s *Server) liveGrantLocked(g Grant) *peerConn {
	k, err := ParseKey(g.WireGuardPublicKey)
	if err != nil {
		return nil
	}
	for _, peer := range s.peers {
		if peer.key == k && grantID(peer.grant) == grantID(g) {
			return peer
		}
	}
	return nil
}
