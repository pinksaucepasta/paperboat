package peersessions

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/relaylimits"

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
	digest     [32]byte  // answerDigest of the answer
	at         time.Time // when it was signed
}

// maxSentConfigurations bounds memory like maxCachedPolicies: about 100 bytes per peer, ~9 kB per 88-peer endpoint.
const maxSentConfigurations = maxCachedPolicies

// answer records claims (signed as token) as sent to endpoint and returns the delta from base when base is the
// configuration last sent to it, else nil (send the full token). Callers hold the endpoint's build lock.
func (s *sentConfigurations) answer(endpoint string, base uint64, token string, claims peernetwork.Configuration, digest [32]byte, now time.Time) *peernetwork.ConfigurationDelta {
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
	s.sent[endpoint] = sentConfiguration{generation: claims.Generation, peers: peernetwork.PeerDigests(claims.Peers), digest: digest, at: now}
	return delta
}

// unchanged reports whether endpoint holds, at base, the answer this replica last signed for it, still at the current
// generation, with the same digest, and young enough that its relay grants (relaylimits.MaxGrantLifetime) stay valid
// until the endpoint's next renewal: then the answer is "unchanged" and nothing is signed.
func (s *sentConfigurations) unchanged(endpoint string, base, current uint64, digest [32]byte, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.sent[endpoint]
	return ok && base == last.generation && current == last.generation && digest == last.digest &&
		now.Sub(last.at) >= 0 && now.Sub(last.at) < relaylimits.MaxGrantLifetime/2
}

// answerDigest is everything an answer is signed from but its generation and timestamps: the claims, the relay nodes
// its candidates and grants name, and the reported home relay.
func answerDigest(claims peernetwork.Configuration, nodes []relayNodeRow, homeRelay string) [32]byte {
	claims.Generation, claims.IssuedAt, claims.ExpiresAt = 0, 0, 0
	h := sha256.New()
	_ = json.NewEncoder(h).Encode(claims)
	for _, n := range nodes {
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00%s\x00%s\x00%d\x00%d\x00%x\x00%t\x00%t\x00%x\x00%x\x00%v\x00%s\x00%s\n", n.id, n.nodeGeneration, n.processEpoch, n.region+"/"+n.failureDomain, n.host, n.tcpPort, n.quicPort, n.pin,
			n.relay, n.peerRelay, n.peerRelayWG, n.peerRelayDisco, n.peerRelayAddress, n.roles, n.transports)
	}
	h.Write([]byte(homeRelay))
	var out [32]byte
	h.Sum(out[:0])
	return out
}
