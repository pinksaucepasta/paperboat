package tailnet

import (
	"bytes"
	"crypto/sha256"

	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// MaxCarrierOfferBytes bounds a peer entry's carrier offer: the native offer's own encoding bound.
const MaxCarrierOfferBytes = 8 << 10

// PeerCarrierOffer is the carrier generation offer the signed configuration carries for endpointID, or nil.
// The configuration only transports it; native verifies it against the peer's binding. An offer declined
// here (a dial on it failed) stays declined until the configuration carries a different one, so a dead
// generation costs one attempt, not one per dial.
func (a *Authority) PeerCarrierOffer(endpointID string) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.usableLocked() {
		return nil
	}
	for _, peer := range a.current.Peers {
		if peer.Identity.EndpointID != endpointID || peerAdmissionExpiry(peer) <= serverclock.Now().Unix() || len(peer.CarrierOffer) == 0 {
			continue
		}
		if declined, ok := a.declinedOffers[endpointID]; ok && declined == sha256.Sum256(peer.CarrierOffer) {
			return nil
		}
		return bytes.Clone(peer.CarrierOffer)
	}
	return nil
}

// DeclineCarrierOffer withholds offer for endpointID from PeerCarrierOffer. One entry per current peer.
func (a *Authority) DeclineCarrierOffer(endpointID string, offer []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current == nil {
		return
	}
	current := make(map[string][32]byte, len(a.declinedOffers)+1)
	for _, peer := range a.current.Peers {
		if hash, ok := a.declinedOffers[peer.Identity.EndpointID]; ok {
			current[peer.Identity.EndpointID] = hash
		}
	}
	current[endpointID] = sha256.Sum256(offer)
	a.declinedOffers = current
}
