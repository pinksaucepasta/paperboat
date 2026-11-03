package derpquic

import (
	"crypto/sha256"
	"io"

	"go4.org/mem"
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
	encoded := root.Raw32()
	digest := sha256.Sum256(append(encoded[:], namespace[:]...))
	return key.NodePublicFromRaw32(mem.B(digest[:]))
}

func writeAuthFrame(w io.Writer, token []byte, namespace [32]byte, disco key.DiscoPublic) error {
	kind, body := authFrame(token, namespace, disco)
	return writeFrame(w, kind, body)
}
func authFrame(token []byte, namespace [32]byte, disco key.DiscoPublic) (byte, []byte) {
	if namespace == ([32]byte{}) {
		return frameAuth, token
	}
	encoded := disco.Raw32()
	return frameAuthGeneration, append(append(namespace[:], encoded[:]...), token...)
}

func (s *Server) authenticate(p *peerConn, kind byte, body []byte) error {
	var namespace [32]byte
	var disco key.DiscoPublic
	switch kind {
	case frameAuth:
	case frameAuthGeneration:
		if len(body) <= 64 {
			return ErrProtocol
		}
		copy(namespace[:], body[:32])
		disco = key.DiscoPublicFromRaw32(mem.B(body[32:64]))
		body = body[64:]
		if namespace == ([32]byte{}) || disco.IsZero() {
			return ErrProtocol
		}
	default:
		return ErrProtocol
	}
	if !p.key.IsZero() && (p.namespace != namespace || p.generationDisco != disco) {
		return ErrAdmission
	}
	if p.key.IsZero() {
		p.namespace = namespace
		p.generationDisco = disco
	}
	return s.admit(p, string(body))
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
