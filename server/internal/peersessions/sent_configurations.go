package peersessions

import (
	"sync"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// sentConfigurations remembers, per endpoint, the generation and peer digests of the last configuration this
// replica signed for it, so the next answer can be a delta (peernetwork.ConfigurationDelta). Like headscale's
// per-node "last sent peers" (hscontrol/mapper/batcher.go computePeerDiff) it lives in memory only: a restart, another
// replica or an endpoint that missed an answer finds no matching base and gets the full configuration.
type sentConfigurations struct {
	mu   sync.Mutex
	sent map[string]sentConfiguration
}

type sentConfiguration struct {
	generation uint64
	peers      map[string][32]byte
}

// maxSentConfigurations bounds memory like maxCachedPolicies: about 100 bytes per peer, ~9 kB per 88-peer endpoint.
const maxSentConfigurations = maxCachedPolicies

// answer records claims (signed as token) as sent to endpoint and returns the delta from base when base is the
// configuration last sent to it, else nil (send the full token). Callers hold the endpoint's build lock.
func (s *sentConfigurations) answer(endpoint string, base uint64, token string, claims peernetwork.Configuration) *peernetwork.ConfigurationDelta {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sent == nil {
		s.sent = map[string]sentConfiguration{}
	}
	var delta *peernetwork.ConfigurationDelta
	if last, ok := s.sent[endpoint]; ok && base != 0 && last.generation == base {
		d := peernetwork.NewConfigurationDelta(token, claims, base, last.peers)
		delta = &d
	}
	if _, ok := s.sent[endpoint]; !ok && len(s.sent) >= maxSentConfigurations {
		for id := range s.sent { // map order: an arbitrary victim, which then gets one full configuration
			delete(s.sent, id)
			break
		}
	}
	s.sent[endpoint] = sentConfiguration{generation: claims.Generation, peers: peernetwork.PeerDigests(claims.Peers)}
	return delta
}
