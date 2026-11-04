package peersessions

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"time"
)

// MaxCarrierOfferBytes bounds a reported carrier offer: the native offer's own encoding bound.
const MaxCarrierOfferBytes = 8 << 10

// carrierOfferLifetime is the native delegation's longest lifetime; a report expiring later is refused.
const carrierOfferLifetime = 5 * time.Minute

// CarrierOfferReport is a machine's active carrier generation offer (native.CarrierOffer bytes, signed by the
// machine's QUIC key), or an empty Offer to withdraw it. The identity comes from the verified machine proof.
type CarrierOfferReport struct {
	UserID            string
	MachineID         string
	MachineGeneration int64
	Offer             []byte
}

// carrierOfferBinding is the part of an offer the server checks: the machine binding and expiry. Clients verify
// the signature and the rest against the same binding; a machine can only describe its own generation.
type carrierOfferBinding struct {
	Claims struct {
		Kind              string
		AccountID         string
		MachineID         string
		MachineGeneration int64
		ParentPublicKey   string
		ExpiresUnix       int64
	}
	Signature []byte
}

// ReportCarrierOffer stores the machine's offer on its identity, replacing the last one, so every peer's next
// configuration carries it (migration 218 publishes peer_network at once). An offer bound to another account,
// machine, installation generation or QUIC key than the authenticated machine's current identity is refused.
func (s *NetworkService) ReportCarrierOffer(ctx context.Context, in CarrierOfferReport) error {
	if ctx == nil || !bounded(in.UserID, 1, 256) || !bounded(in.MachineID, 1, 128) || in.MachineGeneration < 1 || len(in.Offer) > MaxCarrierOfferBytes {
		return ErrInvalid
	}
	now := s.now().UTC()
	var offer, parentKey []byte
	var expires *time.Time
	if len(in.Offer) > 0 {
		var o carrierOfferBinding
		if json.NewDecoder(bytes.NewReader(in.Offer)).Decode(&o) != nil || len(o.Signature) != ed25519.SignatureSize {
			return ErrInvalid
		}
		c := o.Claims
		key, err := base64.RawURLEncoding.Strict().DecodeString(c.ParentPublicKey)
		if err != nil || len(key) != ed25519.PublicKeySize || c.Kind != "paperboat.terminal-carrier/v1" || c.ExpiresUnix <= now.Unix() || c.ExpiresUnix > now.Add(carrierOfferLifetime+time.Minute).Unix() {
			return ErrInvalid
		}
		if c.AccountID != in.UserID || c.MachineID != in.MachineID || c.MachineGeneration != in.MachineGeneration {
			return ErrDenied
		}
		at := time.Unix(c.ExpiresUnix, 0).UTC()
		offer, parentKey, expires = in.Offer, key, &at
	}
	result, err := s.store.SQL().ExecContext(ctx, `UPDATE paperboat.peer_network_identities n SET carrier_offer=$4,carrier_offer_expires_at=$5
 WHERE n.user_id=$1 AND n.endpoint_id=$2 AND n.role='machine' AND n.machine_id=$2 AND n.machine_generation=$3 AND n.revoked_at IS NULL
 AND ($4::bytea IS NULL OR EXISTS(SELECT 1 FROM paperboat.peer_endpoint_certificates c WHERE c.fingerprint=n.quic_certificate_fingerprint
  AND c.user_id=n.user_id AND c.endpoint_id=n.endpoint_id AND c.quic_public_key=$6 AND c.revoked_at IS NULL AND c.expires_at>$7))`,
		in.UserID, in.MachineID, in.MachineGeneration, offer, expires, parentKey, now)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrDenied
	}
	return nil
}

// deliverCarrierOffer moves a peer's unexpired carrier offer from its identity, where loading and the mesh log
// keep it, onto its entry; it is never part of a signed identity.
func deliverCarrierOffer(p *networkPeer, now time.Time) {
	if p.Identity.CarrierOfferExpires > now.Unix() {
		p.CarrierOffer = p.Identity.CarrierOffer
	}
	p.Identity.CarrierOffer, p.Identity.CarrierOfferExpires = nil, 0
}
