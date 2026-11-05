package peersessions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// MaximumDaemonGenerations bounds the live network identities of one CLI session: the running daemon generation
// and the older ones still draining what they hold, as the carrier generations are bounded (carrierSlots).
// Registering one more evicts a generation that can hold nothing (makeGenerationRoom) or is refused.
const MaximumDaemonGenerations = 4

// ErrGenerationEvicted refuses a daemon generation that a newer one pushed out of MaximumDaemonGenerations;
// ErrGenerationWithdrawn one that withdrew or outlived its drain (runtimeattachment.DrainLimit). ErrGenerationLimit
// refuses registering a generation while MaximumDaemonGenerations others may still hold sessions.
var (
	ErrGenerationLimit     = errors.New("register generation: the most generations allowed are live and may still hold connections")
	ErrGenerationEvicted   = fmt.Errorf("%w: a newer daemon generation replaced this one", ErrDenied)
	ErrGenerationWithdrawn = fmt.Errorf("%w: this daemon generation has finished", ErrDenied)
)

// generationReasons are the revocation reasons of a daemon generation's own lifecycle. Only these may be revived
// by a later registration of the same generation (the same binary started again); any other revocation (ended
// session, removed machine) stays.
var generationReasons = map[string]error{"withdrawn": ErrGenerationWithdrawn, "drain_expired": ErrGenerationWithdrawn, "generation_evicted": ErrGenerationEvicted}

func (s *NetworkService) registerAttempt(ctx context.Context, in NetworkRegistration) (NetworkRegistrationResult, error) {
	if ctx == nil || !bounded(in.OperationID, 8, 128) || !bounded(in.UserID, 1, 256) || !bounded(in.EndpointID, 1, 128) || !bounded(in.OwnerEndpointID, 1, 128) || !validWireGuardPublicKey(in.WireGuardPublicKey) || !validWireGuardPublicKey(in.DiscoPublicKey) || len(in.QUICCertificateFingerprint) != 32 || in.ExpectedKeyGeneration < 0 || (in.Role != "cli" && in.Role != "machine") {
		return NetworkRegistrationResult{}, ErrInvalid
	}
	if in.Role == "cli" && (in.MachineID != "" || in.MachineGeneration != 0 || in.OwnerEndpointID == in.EndpointID) || in.Role == "machine" && (in.MachineID != in.EndpointID || in.OwnerEndpointID != in.EndpointID || in.MachineGeneration < 1) {
		return NetworkRegistrationResult{}, ErrInvalid
	}
	hash := sha256.Sum256([]byte(in.UserID + "\x00" + in.EndpointID + "\x00" + in.Role + "\x00" + fmt.Sprint(in.EndpointGeneration) + "\x00" + fmt.Sprint(in.MachineGeneration) + "\x00" + fmt.Sprint(in.ExpectedKeyGeneration) + "\x00" + base64.RawURLEncoding.EncodeToString(in.WireGuardPublicKey) + "\x00" + base64.RawURLEncoding.EncodeToString(in.DiscoPublicKey) + "\x00" + hex.EncodeToString(in.QUICCertificateFingerprint)))
	tx, err := db.BeginSerializable(ctx, s.store.SQL())
	if err != nil {
		return NetworkRegistrationResult{}, err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	if err := checkRegistrationOwner(ctx, tx, in, now); err != nil {
		return NetworkRegistrationResult{}, err
	}
	var oldHash []byte
	var replayGen int64
	err = tx.QueryRowContext(ctx, `SELECT request_hash,key_generation FROM paperboat.peer_network_registration_operations WHERE operation_id=$1`, in.OperationID).Scan(&oldHash, &replayGen)
	if err == nil {
		if !equalBytes(oldHash, hash[:]) {
			return NetworkRegistrationResult{}, ErrConflict
		}
		var address string
		if err = tx.QueryRowContext(ctx, `SELECT host(virtual_address) FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2 AND revoked_at IS NULL`, in.UserID, in.EndpointID).Scan(&address); err != nil {
			return NetworkRegistrationResult{}, unavailableWithCause(err)
		}
		return NetworkRegistrationResult{KeyGeneration: replayGen, VirtualAddress: address}, nil
	}
	if err != sql.ErrNoRows {
		return NetworkRegistrationResult{}, err
	}
	currentGen, address, created, rotated, err := upsertNetworkIdentity(ctx, tx, in, now)
	if err != nil {
		return NetworkRegistrationResult{}, err
	}
	var evicted []string
	if in.Role == "cli" {
		if evicted, err = makeGenerationRoom(ctx, tx, in, now); err != nil {
			return NetworkRegistrationResult{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO paperboat.peer_network_registration_operations(operation_id,user_id,endpoint_id,request_hash,key_generation,created_at) VALUES($1,$2,$3,$4,$5,$6)`, in.OperationID, in.UserID, in.EndpointID, hash[:], currentGen, now); err != nil {
		return NetworkRegistrationResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM paperboat.peer_network_registration_operations o WHERE o.user_id=$1 AND o.endpoint_id=$2 AND o.operation_id IN (SELECT operation_id FROM paperboat.peer_network_registration_operations WHERE user_id=$1 AND endpoint_id=$2 ORDER BY key_generation DESC,created_at DESC,operation_id DESC OFFSET 64)`, in.UserID, in.EndpointID); err != nil {
		return NetworkRegistrationResult{}, err
	}
	eventType := "peer_network.key_rotated"
	if created {
		eventType = "peer_network.key_registered"
	} else if !rotated {
		eventType = "peer_network.certificate_rebound"
	}
	auditID := "aud_peer_network_" + hex.EncodeToString(hash[:16])
	metadata, _ := json.Marshal(map[string]any{"endpoint_id": in.EndpointID, "role": in.Role, "key_generation": currentGen, "virtual_address": address, "evicted_endpoints": evicted})
	if _, err = tx.ExecContext(ctx, `INSERT INTO paperboat.audit_events(id,actor_user_id,actor_type,event_type,resource_type,resource_id,idempotency_key,metadata,created_at) VALUES($1,$2,'user',$3,'peer_network_identity',$4,$5,$6::jsonb,$7) ON CONFLICT (event_type,resource_type,resource_id,idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`, auditID, in.UserID, eventType, in.EndpointID, in.OperationID, metadata, now); err != nil {
		return NetworkRegistrationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return NetworkRegistrationResult{}, err
	}
	for _, endpoint := range evicted {
		slog.Info("dead or drained-out generation evicted", "endpoint_id", endpoint, "by", in.EndpointID, "limit", MaximumDaemonGenerations)
	}
	return NetworkRegistrationResult{KeyGeneration: currentGen, VirtualAddress: address, EvictedEndpoints: evicted}, nil
}

// checkRegistrationOwner admits only an owner that is live now with a current certificate: the CLI session's
// (every daemon generation of it presents the session's certificate) or the machine's installation.
func checkRegistrationOwner(ctx context.Context, tx *sql.Tx, in NetworkRegistration, now time.Time) error {
	var certRole, certEndpoint string
	var certGeneration int64
	err := tx.QueryRowContext(ctx, `SELECT c.role,c.endpoint_id,c.generation FROM paperboat.peer_endpoint_certificates c WHERE c.user_id=$1 AND c.fingerprint=$2 AND c.revoked_at IS NULL AND c.issued_at<=$3 AND c.expires_at>$3`, in.UserID, in.QUICCertificateFingerprint, now).Scan(&certRole, &certEndpoint, &certGeneration)
	if err == sql.ErrNoRows || err == nil && (certRole != in.Role || certEndpoint != in.OwnerEndpointID || certGeneration != in.EndpointGeneration) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if in.Role == "cli" {
		var active bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.cli_client_sessions c JOIN paperboat.users u ON u.id=c.user_id AND u.status='active' WHERE c.id=$1 AND c.user_id=$2 AND c.state='active' AND c.revoked_at IS NULL)`, in.OwnerEndpointID, in.UserID).Scan(&active)
		if err != nil {
			return err
		}
		if !active {
			return ErrDenied
		}
		return nil
	}
	var generation int64
	err = tx.QueryRowContext(ctx, `SELECT m.installation_generation FROM paperboat.user_machines m JOIN paperboat.users u ON u.id=m.user_id AND u.status='active' JOIN paperboat.control_environments e ON e.id=m.environment_id AND e.owner_user_id=m.user_id AND e.desired_state='active' AND e.revoked_at IS NULL WHERE m.id=$1 AND m.user_id=$2 AND m.state IN ('online','offline') AND m.seat_state='occupied' AND m.revoked_at IS NULL AND m.deleted_at IS NULL`, in.EndpointID, in.UserID).Scan(&generation)
	if err == sql.ErrNoRows || err == nil && generation != in.MachineGeneration {
		return ErrDenied
	}
	return err
}

// upsertNetworkIdentity creates the identity or rebinds its key. A daemon generation revoked by its own lifecycle
// (withdrawn, drained out, evicted) is revived when the same generation starts again; it counts as new then
// (created_at), so the eviction order stays oldest first.
func upsertNetworkIdentity(ctx context.Context, tx *sql.Tx, in NetworkRegistration, now time.Time) (currentGen int64, address string, created, rotated bool, err error) {
	var currentWireGuard, currentDisco []byte
	var revoked bool
	var reason sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT key_generation,host(virtual_address),wireguard_public_key,disco_public_key,revoked_at IS NOT NULL,revocation_reason FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2 AND role=$3 FOR UPDATE`, in.UserID, in.EndpointID, in.Role).Scan(&currentGen, &address, &currentWireGuard, &currentDisco, &revoked, &reason)
	if err == sql.ErrNoRows {
		if in.ExpectedKeyGeneration != 0 {
			return 0, "", false, false, ErrConflict
		}
		err = tx.QueryRowContext(ctx, `INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,cli_client_session_id,role,machine_id,endpoint_generation,machine_generation,key_generation,wireguard_public_key,disco_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at) VALUES($1,$2,nullif($3,$2),$4,nullif($5,''),$6,$7,1,$8,$9,$10,('fd7a:115c:a1e0::'::inet + nextval('paperboat.peer_network_virtual_address_seq')), $11,$11) RETURNING key_generation,host(virtual_address)`, in.UserID, in.EndpointID, in.OwnerEndpointID, in.Role, in.MachineID, in.EndpointGeneration, in.MachineGeneration, in.WireGuardPublicKey, in.DiscoPublicKey, in.QUICCertificateFingerprint, now).Scan(&currentGen, &address)
		return currentGen, address, true, true, err
	}
	if err != nil {
		return 0, "", false, false, err
	}
	if revoked && (in.Role != "cli" || generationReasons[reason.String] == nil) {
		return 0, "", false, false, ErrDenied
	}
	if currentGen != in.ExpectedKeyGeneration {
		return 0, "", false, false, ErrConflict
	}
	rotated = !equalBytes(currentWireGuard, in.WireGuardPublicKey) || !equalBytes(currentDisco, in.DiscoPublicKey)
	if rotated {
		currentGen++
	}
	_, err = tx.ExecContext(ctx, `UPDATE paperboat.peer_network_identities SET endpoint_generation=$1,machine_generation=$2,key_generation=$3,wireguard_public_key=$4,disco_public_key=$5,quic_certificate_fingerprint=$6,revoked_at=NULL,revocation_reason=NULL,drain_until=NULL,created_at=CASE WHEN revoked_at IS NULL THEN created_at ELSE $7 END,updated_at=$7 WHERE user_id=$8 AND endpoint_id=$9`, in.EndpointGeneration, in.MachineGeneration, currentGen, in.WireGuardPublicKey, in.DiscoPublicKey, in.QUICCertificateFingerprint, now, in.UserID, in.EndpointID)
	return currentGen, address, false, rotated || revoked, err
}

// deadGenerationAfter is how long a generation may go without renewing before it counts as dead: a live one,
// draining or not, renews its configuration about every minute (admission.DefaultLifetime is 2 min).
const deadGenerationAfter = 15 * time.Minute

// makeGenerationRoom keeps at most MaximumDaemonGenerations live generations of the owner with the registering one.
// It revokes only generations that can hold no session: past their drain_until, or dead (not renewed for
// deadGenerationAfter), oldest first. When the others may all still hold sessions it refuses the registration
// (ErrGenerationLimit) instead of cutting one.
func makeGenerationRoom(ctx context.Context, tx *sql.Tx, in NetworkRegistration, now time.Time) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT endpoint_id,(drain_until<$4 OR updated_at<$5) FROM paperboat.peer_network_identities
 WHERE user_id=$1 AND owner_endpoint_id=$2 AND role=$3 AND revoked_at IS NULL AND endpoint_id<>$6
 ORDER BY created_at,endpoint_id FOR UPDATE`, in.UserID, in.OwnerEndpointID, in.Role, now, now.Add(-deadGenerationAfter), in.EndpointID)
	if err != nil {
		return nil, err
	}
	var others int
	var evictable []string
	for rows.Next() {
		var endpoint string
		var gone sql.NullBool
		if err := rows.Scan(&endpoint, &gone); err != nil {
			rows.Close()
			return nil, err
		}
		others++
		if gone.Bool {
			evictable = append(evictable, endpoint)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	excess := others - (MaximumDaemonGenerations - 1)
	if excess <= 0 {
		return nil, nil
	}
	if excess > len(evictable) {
		return nil, ErrGenerationLimit
	}
	evicted := evictable[:excess]
	if _, err := tx.ExecContext(ctx, `UPDATE paperboat.peer_network_identities SET revoked_at=$3,revocation_reason='generation_evicted',updated_at=$3 WHERE user_id=$1 AND endpoint_id=ANY($2)`, in.UserID, evicted, now); err != nil {
		return nil, err
	}
	return evicted, nil
}

// BeginDrain records that a daemon generation stopped accepting and drains what it holds, for at most
// runtimeattachment.DrainLimit; retention revokes it after that. Repeating it keeps the first deadline.
func (s *NetworkService) BeginDrain(ctx context.Context, userID, ownerEndpointID, endpointID string) error {
	return s.generationLifecycle(ctx, userID, ownerEndpointID, endpointID,
		`UPDATE paperboat.peer_network_identities SET drain_until=coalesce(drain_until,$4) WHERE user_id=$1 AND owner_endpoint_id=$2 AND endpoint_id=$3 AND role='cli' AND revoked_at IS NULL`,
		s.now().UTC().Add(runtimewire.DrainLimit))
}

// Withdraw revokes a daemon generation that finished draining. Withdrawing a generation already revoked succeeds.
func (s *NetworkService) Withdraw(ctx context.Context, userID, ownerEndpointID, endpointID string) error {
	return s.generationLifecycle(ctx, userID, ownerEndpointID, endpointID,
		`UPDATE paperboat.peer_network_identities SET revoked_at=$4,revocation_reason='withdrawn',updated_at=$4 WHERE user_id=$1 AND owner_endpoint_id=$2 AND endpoint_id=$3 AND role='cli' AND revoked_at IS NULL`,
		s.now().UTC())
}

func (s *NetworkService) generationLifecycle(ctx context.Context, userID, ownerEndpointID, endpointID, statement string, at time.Time) error {
	if ctx == nil || !bounded(userID, 1, 256) || !bounded(ownerEndpointID, 1, 128) || !bounded(endpointID, 1, 128) || ownerEndpointID == endpointID {
		return ErrInvalid
	}
	return db.RetrySerializable(ctx, func() error {
		_, err := s.store.SQL().ExecContext(ctx, statement, userID, ownerEndpointID, endpointID, at)
		return err
	})
}

// refusedGeneration names why a CLI generation's identity is refused, when its own lifecycle revoked it.
func refusedGeneration(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, userID, endpointID string) error {
	var reason sql.NullString
	err := q.QueryRowContext(ctx, `SELECT revocation_reason FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2 AND role='cli' AND revoked_at IS NOT NULL`, userID, endpointID).Scan(&reason)
	if refused := generationReasons[reason.String]; err == nil && refused != nil {
		return refused
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return ErrDenied
}

// ownerEndpoints lists the live network identities of an owner: the machine itself, or every live daemon
// generation of a CLI session (at most MaximumDaemonGenerations).
func ownerEndpoints(ctx context.Context, tx *sql.Tx, userID, owner string, cli bool) ([]string, error) {
	if !cli {
		return []string{owner}, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT endpoint_id FROM paperboat.peer_network_identities WHERE user_id=$1 AND owner_endpoint_id=$2 AND role='cli' AND revoked_at IS NULL ORDER BY endpoint_id LIMIT $3`, userID, owner, MaximumDaemonGenerations)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
