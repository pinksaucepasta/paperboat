package peernetwork

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
)

// ConfigurationDelta is a signed configuration sent as its changes from the one the endpoint holds, so a peer
// joining an 88-machine account does not cost every endpoint its whole ~100 KB document (PLAN 3b "Peer network
// deltas"). Prior art: Tailscale's MapResponse carries PeersChanged/PeersRemoved against the netmap the client holds
// (tailcfg/tailcfg.go MapResponse; control/controlclient/map.go mapSession.updateStateFromResponse), and headscale
// diffs against the peers it last sent a node (hscontrol/mapper/batcher.go computePeerDiff). The signature is the
// full configuration's: the receiver rebuilds the exact token from the one it holds (Rebuild) and verifies it like any
// other, so a delta carries no authority of its own and a wrong rebuild fails verification.
type ConfigurationDelta struct {
	// BaseGeneration is the generation of the configuration the changes apply to.
	BaseGeneration uint64 `json:"base_generation"`
	// Header and Signature are the first and third parts of the full token.
	Header    string `json:"header"`
	Signature string `json:"signature"`
	// Configuration is the full claims except Peers, which holds only the added and changed peers.
	Configuration Configuration `json:"configuration"`
	// Removed are the endpoint ids of the base's peers that are gone.
	Removed []string `json:"removed"`
}

// PeerDigests is what a sender keeps of a configuration it sent to diff the next one against: each peer's digest.
func PeerDigests(peers []Peer) map[string][32]byte {
	out := make(map[string][32]byte, len(peers))
	for _, p := range peers {
		raw, _ := json.Marshal(p) // a Peer always marshals
		out[p.Identity.EndpointID] = sha256.Sum256(raw)
	}
	return out
}

// NewConfigurationDelta is the delta from the configuration whose PeerDigests are sent (generation base) to claims,
// whose signed token is token. Peers of claims must be ordered by endpoint id, as Rebuild orders them.
func NewConfigurationDelta(token string, claims Configuration, base uint64, sent map[string][32]byte) ConfigurationDelta {
	parts := strings.Split(token, ".")
	d := ConfigurationDelta{BaseGeneration: base, Header: parts[0], Signature: parts[len(parts)-1], Configuration: claims, Removed: []string{}}
	d.Configuration.Peers = []Peer{}
	now := PeerDigests(claims.Peers)
	for _, p := range claims.Peers {
		if digest, held := sent[p.Identity.EndpointID]; !held || digest != now[p.Identity.EndpointID] {
			d.Configuration.Peers = append(d.Configuration.Peers, p)
		}
	}
	for id := range sent {
		if _, kept := now[id]; !kept {
			d.Removed = append(d.Removed, id)
		}
	}
	slices.Sort(d.Removed)
	return d
}

// Rebuild applies the delta to held, the signed configuration token the endpoint holds, and returns the full token
// the server signed. The result is unverified: the caller verifies it as any received configuration.
func (d ConfigurationDelta) Rebuild(held string) (string, error) {
	base, err := tokenClaims(held)
	if err != nil || base.Generation != d.BaseGeneration || d.BaseGeneration == 0 {
		return "", invalid("configuration delta base")
	}
	peers := make(map[string]Peer, len(base.Peers))
	for _, p := range base.Peers {
		peers[p.Identity.EndpointID] = p
	}
	for _, id := range d.Removed {
		if _, ok := peers[id]; !ok {
			return "", invalid("configuration delta removes an unknown peer")
		}
		delete(peers, id)
	}
	for _, p := range d.Configuration.Peers {
		peers[p.Identity.EndpointID] = p
	}
	if len(peers) > MaxEndpoints {
		return "", invalid("configuration delta peers")
	}
	full := d.Configuration
	full.Peers = make([]Peer, 0, len(peers))
	for _, p := range peers {
		full.Peers = append(full.Peers, p)
	}
	slices.SortFunc(full.Peers, func(a, b Peer) int { return strings.Compare(a.Identity.EndpointID, b.Identity.EndpointID) })
	payload, err := json.Marshal(full)
	if err != nil {
		return "", err
	}
	return d.Header + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + d.Signature, nil
}

// ConfigurationGeneration is the generation a configuration token claims, 0 when it holds none readable. It reads
// the claims without verifying them: a sender answers a wrong base with a full configuration.
func ConfigurationGeneration(token string) uint64 {
	c, err := tokenClaims(token)
	if err != nil {
		return 0
	}
	return c.Generation
}

func tokenClaims(token string) (Configuration, error) {
	var c Configuration
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > MaxConfigurationBytes {
		return c, invalid("configuration token")
	}
	body, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return c, invalid("configuration token")
	}
	err = DecodeSigned(body, &c)
	return c, err
}
