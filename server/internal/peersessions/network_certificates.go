package peersessions

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"time"
)

// AdmittedCertificate identifies a certificate pinned by an actual live carrier.
// It never permits a fresh TLS handshake with an expired or superseded certificate.
type AdmittedCertificate struct {
	EndpointID  string `json:"endpoint_id"`
	Fingerprint string `json:"fingerprint"`
}

func validateAdmittedCertificates(refs []AdmittedCertificate, hasScopes bool) error {
	if len(refs) > 128 || len(refs) > 0 && !hasScopes {
		return ErrInvalid
	}
	seen := map[AdmittedCertificate]bool{}
	for _, ref := range refs {
		raw, err := hex.DecodeString(ref.Fingerprint)
		if !bounded(ref.EndpointID, 1, 128) || err != nil || len(raw) != 32 || hex.EncodeToString(raw) != ref.Fingerprint || seen[ref] {
			return ErrInvalid
		}
		seen[ref] = true
	}
	return nil
}

func retainAdmittedCertificates(ctx context.Context, tx *sql.Tx, self *networkBinding, peers map[string]*networkPeer, refs []AdmittedCertificate, now time.Time) error {
	for _, ref := range refs {
		binding := self
		if ref.EndpointID != self.EndpointID {
			peer := peers[ref.EndpointID]
			if peer == nil {
				continue
			} // Current scope withdrawal also withdraws old bindings.
			binding = &peer.Identity
		}
		fingerprint, _ := hex.DecodeString(ref.Fingerprint)
		key, err := base64.RawURLEncoding.DecodeString(binding.QUICPublicKey)
		if err != nil {
			return ErrUnavailable
		}
		var allowed bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM paperboat.peer_endpoint_certificates old
JOIN paperboat.peer_endpoint_certificates current ON current.fingerprint=decode($7,'hex')
WHERE old.fingerprint=$1 AND old.user_id=$2 AND old.endpoint_id=$3
AND old.role=$4 AND old.generation=$5 AND old.quic_public_key=$6
AND old.signer_public_key=current.signer_public_key AND old.issued_at<=$8
AND (old.revoked_at IS NULL OR old.revocation_reason='certificate_superseded')
AND NOT EXISTS(SELECT 1 FROM paperboat.peer_endpoint_certificate_revocations r WHERE r.user_id=old.user_id AND r.certificate_fingerprint=old.fingerprint))`, fingerprint, binding.AccountID, binding.EndpointID, binding.Role, binding.EndpointGeneration, key, binding.QUICCertificateFingerprint, now).Scan(&allowed)
		if err != nil {
			return err
		}
		if allowed {
			binding.AdmittedCertificateFingerprints = append(binding.AdmittedCertificateFingerprints, ref.Fingerprint)
		}
	}
	return nil
}
