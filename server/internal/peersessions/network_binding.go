package peersessions

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/netip"
	"time"
)

func loadNetworkBinding(ctx context.Context, tx *sql.Tx, userID, endpointID string, now time.Time, admitted bool) (networkBinding, time.Time, error) {
	var b networkBinding
	var key, disco, fingerprint, quicPublic, delivered []byte
	var expiry time.Time
	err := tx.QueryRowContext(ctx, `SELECT n.user_id,n.endpoint_id,n.role,coalesce(n.machine_id,''),n.endpoint_generation,n.machine_generation,n.key_generation,n.wireguard_public_key,n.disco_public_key,n.quic_certificate_fingerprint,c.quic_public_key,host(n.virtual_address),c.expires_at,array_to_json(n.published_endpoints) FROM paperboat.peer_network_identities n JOIN paperboat.users u ON u.id=n.user_id AND u.status='active' JOIN paperboat.peer_endpoint_certificates c ON c.fingerprint=n.quic_certificate_fingerprint AND c.user_id=n.user_id AND c.endpoint_id=n.endpoint_id AND c.generation=n.endpoint_generation AND c.role=n.role
AND (c.revoked_at IS NULL OR ($4 AND c.revocation_reason='certificate_superseded'
 AND EXISTS(SELECT 1 FROM paperboat.peer_endpoint_certificates replacement
 WHERE replacement.user_id=c.user_id AND replacement.endpoint_id=c.endpoint_id
 AND replacement.role=c.role AND replacement.generation=c.generation
 AND replacement.quic_public_key=c.quic_public_key AND replacement.signer_public_key=c.signer_public_key
 AND replacement.revoked_at IS NULL AND replacement.issued_at<=$3 AND replacement.serial>c.serial)))
AND NOT EXISTS(SELECT 1 FROM paperboat.peer_endpoint_certificate_revocations r WHERE r.user_id=c.user_id AND r.certificate_fingerprint=c.fingerprint)
AND c.issued_at<=$3 AND (c.expires_at>$3 OR $4) WHERE n.user_id=$1 AND n.endpoint_id=$2 AND n.revoked_at IS NULL`, userID, endpointID, now, admitted).Scan(&b.AccountID, &b.EndpointID, &b.Role, &b.MachineID, &b.EndpointGeneration, &b.MachineGeneration, &b.KeyGeneration, &key, &disco, &fingerprint, &quicPublic, &b.VirtualAddress, &expiry, &delivered)
	if err != nil {
		return b, time.Time{}, err
	}
	if b.Endpoints, err = publishedEndpoints(delivered); err != nil {
		return b, time.Time{}, err
	}
	if !admitted && expiry.Unix() <= now.Unix() {
		slog.Warn("peer network authority unavailable", "site", "network.go:786")
		return b, time.Time{}, ErrUnavailable
	}
	if b.Role == "cli" {
		var active bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.cli_client_sessions WHERE id=$1 AND user_id=$2 AND state='active' AND revoked_at IS NULL)`, endpointID, userID).Scan(&active)
		if err != nil {
			return b, time.Time{}, err
		}
		if !active {
			slog.Warn("peer network authority unavailable", "site", "network.go:795")
			return b, time.Time{}, ErrUnavailable
		}
	} else {
		var gen int64
		err = tx.QueryRowContext(ctx, `SELECT m.installation_generation FROM paperboat.user_machines m JOIN paperboat.control_environments e ON e.id=m.environment_id AND e.owner_user_id=m.user_id AND e.desired_state='active' AND e.revoked_at IS NULL WHERE m.id=$1 AND m.user_id=$2 AND m.state IN ('online','offline') AND m.seat_state='occupied' AND m.revoked_at IS NULL AND m.deleted_at IS NULL`, endpointID, userID).Scan(&gen)
		if err == sql.ErrNoRows || err == nil && gen != b.MachineGeneration {
			slog.Warn("peer network authority unavailable", "site", "network.go:801")
			return b, time.Time{}, ErrUnavailable
		}
		if err != nil {
			return b, time.Time{}, err
		}
	}
	b.WireGuardPublicKey = base64.RawURLEncoding.EncodeToString(key)
	if len(disco) == 32 {
		b.DiscoPublicKey = base64.RawURLEncoding.EncodeToString(disco)
	}
	b.QUICCertificateFingerprint = hex.EncodeToString(fingerprint)
	if len(quicPublic) != 32 {
		slog.Warn("peer network authority unavailable", "site", "network.go:813")
		return b, time.Time{}, ErrUnavailable
	}
	b.QUICPublicKey = base64.RawURLEncoding.EncodeToString(quicPublic)
	if _, err = netip.ParseAddr(b.VirtualAddress); err != nil {
		slog.Warn("peer network authority unavailable", "site", "network.go:817")
		return b, time.Time{}, ErrUnavailable
	}
	return b, expiry, nil
}

func bindingMap(b networkBinding) map[string]any {
	raw, _ := json.Marshal(b)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}
