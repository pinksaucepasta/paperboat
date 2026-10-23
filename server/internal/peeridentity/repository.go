package peeridentity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	wire "github.com/pinksaucepasta/paperboat/shared/peeridentity"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

type SQLRepository struct {
	store *db.DB
	audit *audit.Writer
}

func (r *SQLRepository) Bootstrap(ctx context.Context, operationID, userID string, rootPublic ed25519.PublicKey, proposed wire.Certificate, now time.Time) (wire.Certificate, error) {
	if r == nil || ctx == nil || now.IsZero() || len(operationID) < 16 || len(operationID) > 256 || !wire.ValidIdentifier(userID) || len(rootPublic) != ed25519.PublicKeySize || proposed.Role != wire.RoleCLI || proposed.EndpointID == "" || proposed.AccountID != userID {
		return wire.Certificate{}, wire.ErrInvalid
	}
	rootFingerprint := sha256.Sum256(rootPublic)
	if proposed.RootFingerprint != rootFingerprint {
		return wire.Certificate{}, wire.ErrIdentity
	}
	var result wire.Certificate
	err := r.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		q := tx.Queries()
		root, err := q.CreateAccountE2EERoot(ctx, dbsqlc.CreateAccountE2EERootParams{UserID: userID, PublicKey: rootPublic, Fingerprint: rootFingerprint[:]})
		created := err == nil
		if errors.Is(err, sql.ErrNoRows) {
			root, err = q.GetAccountE2EERootForUpdate(ctx, userID)
		}
		if err != nil {
			return err
		}
		if root.RevokedAt.Valid || root.Generation != 1 {
			return ErrConflict
		}
		key, err := q.GetAccountE2EEKeyByFingerprintForUpdate(ctx, dbsqlc.GetAccountE2EEKeyByFingerprintForUpdateParams{UserID: userID, Fingerprint: rootFingerprint[:]})
		if errors.Is(err, sql.ErrNoRows) {
			keys, listErr := q.ListAccountE2EEKeys(ctx, userID)
			if listErr != nil {
				return listErr
			}
			if len(keys) != 0 {
				return ErrConflict
			}
			key, err = q.CreateAccountE2EEKey(ctx, dbsqlc.CreateAccountE2EEKeyParams{
				KeyID: keyIDForFingerprint(rootFingerprint), UserID: userID, PublicKey: rootPublic,
				Fingerprint: rootFingerprint[:], Generation: 1, CreatedAt: now, UpdatedAt: now,
			})
			if errors.Is(err, sql.ErrNoRows) {
				key, err = q.GetAccountE2EEKeyByFingerprintForUpdate(ctx, dbsqlc.GetAccountE2EEKeyByFingerprintForUpdateParams{UserID: userID, Fingerprint: rootFingerprint[:]})
			}
		}
		if err != nil {
			return err
		}
		if key.RevokedAt.Valid || !bytes.Equal(key.PublicKey, rootPublic) || !bytes.Equal(key.Fingerprint, rootFingerprint[:]) {
			return ErrConflict
		}
		result, err = r.registerTx(ctx, tx, operationID, userID, proposed, key.PublicKey, false, now)
		if err != nil {
			return err
		}
		if created {
			rootOperation := sha256.Sum256([]byte("account-e2ee-root\x00" + operationID))
			return r.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "account_e2ee.root_created", ResourceType: "account_e2ee_root", ResourceID: hex.EncodeToString(rootFingerprint[:]), IdempotencyKey: "root_" + hex.EncodeToString(rootOperation[:]), Metadata: map[string]any{"generation": 1}})
		}
		return nil
	})
	return result, err
}

// BootstrapFresh registers one machine signing key for the authenticated CLI
// session. The v1 name is retained for compatibility. Active CLI sessions are
// already the result of account authentication and any required setup
// approval. Existing identities remain valid until explicitly revoked.
func (r *SQLRepository) BootstrapFresh(ctx context.Context, operationID, userID, cliSessionID string, rootPublic ed25519.PublicKey, proposed wire.Certificate, now time.Time) (wire.Certificate, error) {
	if r == nil || ctx == nil || now.IsZero() || len(operationID) < 16 || len(operationID) > 256 || !wire.ValidIdentifier(userID) || !wire.ValidIdentifier(cliSessionID) || len(rootPublic) != ed25519.PublicKeySize || proposed.Role != wire.RoleCLI || proposed.EndpointID != cliSessionID || proposed.AccountID != userID {
		return wire.Certificate{}, wire.ErrInvalid
	}
	rootFingerprint := sha256.Sum256(rootPublic)
	if proposed.RootFingerprint != rootFingerprint {
		return wire.Certificate{}, wire.ErrIdentity
	}
	var result wire.Certificate
	err := r.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		q := tx.Queries()
		if _, err := q.GetMachineEnrollmentClientSession(ctx, dbsqlc.GetMachineEnrollmentClientSessionParams{ID: cliSessionID, UserID: userID}); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrConflict
			}
			return err
		}
		var err error
		result, err = r.registerTx(ctx, tx, operationID, userID, proposed, rootPublic, false, now)
		return err
	})
	return result, err
}

func NewSQLRepository(store *db.DB, writer *audit.Writer) (*SQLRepository, error) {
	if store == nil || writer == nil {
		return nil, wire.ErrInvalid
	}
	return &SQLRepository{store: store, audit: writer}, nil
}

func (r *SQLRepository) ResolveAccountRoot(ctx context.Context, userID string) (AccountRoot, error) {
	if r == nil || ctx == nil || !wire.ValidIdentifier(userID) {
		return AccountRoot{}, wire.ErrInvalid
	}
	row, err := r.store.Queries().GetActiveAccountE2EERoot(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountRoot{}, ErrUnavailable
	}
	if err != nil || row.Generation <= 0 {
		if err != nil {
			return AccountRoot{}, err
		}
		return AccountRoot{}, ErrUnavailable
	}
	rows, err := r.store.Queries().ListActiveAccountE2EEKeys(ctx, userID)
	if err != nil || len(rows) == 0 {
		if err != nil {
			return AccountRoot{}, err
		}
		return AccountRoot{}, ErrUnavailable
	}
	keys := make([]AccountKey, 0, len(rows))
	for _, row := range rows {
		key, err := accountKeyFromRow(row)
		if err != nil {
			return AccountRoot{}, err
		}
		keys = append(keys, key)
	}
	return AccountRoot{Keys: keys}, nil
}

// ResolveTransportKeys returns only signers of currently authorized endpoint
// certificates. ENV authority is intentionally separate from this list.
func (r *SQLRepository) ResolveTransportKeys(ctx context.Context, userID string, now time.Time) (AccountRoot, error) {
	if r == nil || ctx == nil || !wire.ValidIdentifier(userID) || now.IsZero() {
		return AccountRoot{}, wire.ErrInvalid
	}
	rows, err := r.store.Queries().ListActivePeerTransportSigners(ctx, dbsqlc.ListActivePeerTransportSignersParams{UserID: userID, Now: now})
	if err != nil {
		return AccountRoot{}, err
	}
	if len(rows) == 0 {
		return AccountRoot{}, ErrUnavailable
	}
	keys := make([]AccountKey, 0, len(rows))
	for _, row := range rows {
		if len(row.SignerPublicKey) != ed25519.PublicKeySize {
			return AccountRoot{}, ErrConflict
		}
		fingerprint := sha256.Sum256(row.SignerPublicKey)
		if row.KeyID != keyIDForFingerprint(fingerprint) {
			return AccountRoot{}, ErrConflict
		}
		keys = append(keys, AccountKey{KeyID: row.KeyID, PublicKey: append(ed25519.PublicKey(nil), row.SignerPublicKey...), Fingerprint: fingerprint, Generation: 1})
	}
	return AccountRoot{Keys: keys}, nil
}

func (r *SQLRepository) MachineTransportSigner(ctx context.Context, userID, machineID string, now time.Time) (AccountKey, error) {
	if r == nil || ctx == nil || !wire.ValidIdentifier(userID) || !wire.ValidIdentifier(machineID) || now.IsZero() {
		return AccountKey{}, wire.ErrInvalid
	}
	var keyID string
	var public []byte
	err := r.store.SQL().QueryRowContext(ctx, `SELECT c.key_id,c.signer_public_key
		FROM paperboat.peer_endpoint_certificates c
		JOIN paperboat.user_machines m ON m.id=c.endpoint_id AND m.user_id=c.user_id AND m.installation_generation=c.generation
		JOIN paperboat.users u ON u.id=m.user_id AND u.status='active'
		JOIN paperboat.control_environments e ON e.id=m.environment_id AND e.owner_user_id=m.user_id AND e.desired_state='active' AND e.revoked_at IS NULL
		WHERE c.user_id=$1 AND c.endpoint_id=$2 AND c.role='machine' AND c.revoked_at IS NULL AND c.issued_at<=$3 AND c.expires_at>$3
		AND m.state IN ('online','offline') AND m.seat_state='occupied'
		AND m.revoked_at IS NULL AND m.deleted_at IS NULL`, userID, machineID, now.UTC()).Scan(&keyID, &public)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountKey{}, ErrUnavailable
	}
	if err != nil {
		return AccountKey{}, err
	}
	if len(public) != ed25519.PublicKeySize {
		return AccountKey{}, ErrConflict
	}
	fingerprint := sha256.Sum256(public)
	if keyID != keyIDForFingerprint(fingerprint) {
		return AccountKey{}, ErrConflict
	}
	return AccountKey{KeyID: keyID, PublicKey: append(ed25519.PublicKey(nil), public...), Fingerprint: fingerprint, Generation: 1}, nil
}

func (r *SQLRepository) RequestMachineEndpoint(ctx context.Context, request MachineEndpointRequest, id string, hash [sha256.Size]byte, expiresAt time.Time) (EndpointEnrollmentRequest, error) {
	if r == nil || ctx == nil || !wire.ValidIdentifier(id) || !expiresAt.After(request.Now) {
		return EndpointEnrollmentRequest{}, wire.ErrInvalid
	}
	var result EndpointEnrollmentRequest
	err := r.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		q := tx.Queries()
		if _, err := q.ExpirePeerEndpointEnrollmentRequests(ctx, request.Now.UTC()); err != nil {
			return err
		}
		existing, err := q.GetPeerEndpointEnrollmentRequestByOperation(ctx, request.OperationID)
		if err == nil {
			matches := bytes.Equal(existing.RequestHash, hash[:]) && existing.UserID == request.UserID && existing.EndpointID == request.EndpointID && existing.Generation == int64(request.Generation)
			if matches && existing.State == "expired" {
				renewed, renewErr := q.RenewExpiredPeerEndpointEnrollmentRequest(ctx, dbsqlc.RenewExpiredPeerEndpointEnrollmentRequestParams{
					CreatedAt: request.Now.UTC(), ExpiresAt: expiresAt.UTC(), OperationKey: request.OperationID,
					RequestHash: hash[:], UserID: request.UserID, EndpointID: request.EndpointID, Generation: int64(request.Generation),
					QuicPublicKey: request.QUICPublicKey[:],
				})
				if errors.Is(renewErr, sql.ErrNoRows) {
					return ErrUnavailable
				}
				if renewErr != nil {
					return renewErr
				}
				result, renewErr = endpointRequestFromRow(renewed)
				if renewErr != nil {
					return renewErr
				}
				return r.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: request.UserID, ActorType: audit.ActorSystem, EventType: "peer_endpoint.enrollment_renewed", ResourceType: "peer_endpoint_enrollment", ResourceID: renewed.ID, IdempotencyKey: request.OperationID + ":renew:" + request.Now.UTC().Format(time.RFC3339Nano), Metadata: map[string]any{"endpoint_id": request.EndpointID, "generation": request.Generation}})
			}
			if matches && existing.State == "fulfilled" {
				// The certificate is already durably registered. Return only the
				// same bound enrollment record so a client that failed before
				// saving it locally can poll and recover; never create another
				// request or certificate for this operation.
				result, err = endpointRequestFromRow(existing)
				return err
			}
			if !matches || existing.State != "pending" || !existing.ExpiresAt.After(request.Now) {
				return ErrConflict
			}
			result, err = endpointRequestFromRow(existing)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		row, err := q.CreatePeerEndpointEnrollmentRequest(ctx, dbsqlc.CreatePeerEndpointEnrollmentRequestParams{ID: id, OperationKey: request.OperationID, RequestHash: hash[:], UserID: request.UserID, EndpointID: request.EndpointID, Generation: int64(request.Generation), QuicPublicKey: request.QUICPublicKey[:], CreatedAt: request.Now.UTC(), ExpiresAt: expiresAt.UTC()})
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnavailable
		}
		if err != nil {
			return err
		}
		result, err = endpointRequestFromRow(row)
		if err != nil {
			return err
		}
		return r.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: request.UserID, ActorType: audit.ActorSystem, EventType: "peer_endpoint.enrollment_requested", ResourceType: "peer_endpoint_enrollment", ResourceID: row.ID, IdempotencyKey: request.OperationID, Metadata: map[string]any{"endpoint_id": request.EndpointID, "generation": request.Generation}})
	})
	return result, err
}

func (r *SQLRepository) RequestCLIEndpoint(ctx context.Context, request CLIEndpointRequest, id string, hash [sha256.Size]byte, expiresAt time.Time) (EndpointEnrollmentRequest, error) {
	if r == nil || ctx == nil || !wire.ValidIdentifier(id) || !expiresAt.After(request.Now) {
		return EndpointEnrollmentRequest{}, wire.ErrInvalid
	}
	var result EndpointEnrollmentRequest
	err := r.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		q := tx.Queries()
		if _, err := q.ExpirePeerEndpointEnrollmentRequests(ctx, request.Now.UTC()); err != nil {
			return err
		}
		existing, err := q.GetPeerEndpointEnrollmentRequestByOperation(ctx, request.OperationID)
		if err == nil {
			matches := bytes.Equal(existing.RequestHash, hash[:]) && existing.UserID == request.UserID && existing.EndpointID == request.EndpointID && existing.Generation == int64(request.Generation) && existing.Role == "cli"
			if !matches {
				return ErrConflict
			}
			if err := requireActiveAccountRoot(ctx, q, request.UserID); err != nil {
				return err
			}
			if matches && existing.State == "expired" {
				renewed, renewErr := q.RenewExpiredCLIPeerEndpointEnrollmentRequest(ctx, dbsqlc.RenewExpiredCLIPeerEndpointEnrollmentRequestParams{CreatedAt: request.Now.UTC(), ExpiresAt: expiresAt.UTC(), OperationKey: request.OperationID, RequestHash: hash[:], UserID: request.UserID, EndpointID: request.EndpointID, Generation: int64(request.Generation), QuicPublicKey: request.QUICPublicKey[:]})
				if errors.Is(renewErr, sql.ErrNoRows) {
					return classifyCLIRenewalNoRows(ctx, q, request)
				}
				if renewErr != nil {
					return renewErr
				}
				result, renewErr = endpointRequestFromRow(renewed)
				if renewErr != nil {
					return renewErr
				}
				return r.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: request.UserID, ActorType: audit.ActorSystem, EventType: "peer_endpoint.cli_enrollment_renewed", ResourceType: "peer_endpoint_enrollment", ResourceID: renewed.ID, IdempotencyKey: request.OperationID + ":renew", Metadata: map[string]any{"endpoint_id": request.EndpointID, "generation": request.Generation, "role": "cli"}})
			}
			if existing.State == "fulfilled" {
				// The certificate is already durably registered. Return the same
				// enrollment record so a client can recover after losing the first
				// response without creating another request or certificate.
				result, err = endpointRequestFromRow(existing)
				return err
			}
			if existing.State != "pending" || !existing.ExpiresAt.After(request.Now) {
				return ErrConflict
			}
			result, err = endpointRequestFromRow(existing)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := requireActiveAccountRoot(ctx, q, request.UserID); err != nil {
			return err
		}
		row, err := q.CreateCLIPeerEndpointEnrollmentRequest(ctx, dbsqlc.CreateCLIPeerEndpointEnrollmentRequestParams{ID: id, OperationKey: request.OperationID, RequestHash: hash[:], UserID: request.UserID, EndpointID: request.EndpointID, Generation: int64(request.Generation), QuicPublicKey: request.QUICPublicKey[:], CreatedAt: request.Now.UTC(), ExpiresAt: expiresAt.UTC()})
		if errors.Is(err, sql.ErrNoRows) {
			// ON CONFLICT can lose either to the same operation racing in another
			// transaction or to a different operation already pending for this
			// endpoint identity. Re-read after PostgreSQL has resolved the conflict
			// so the former is an idempotent replay and the latter is a stable 409.
			winner, winnerErr := q.GetPeerEndpointEnrollmentRequestByOperation(ctx, request.OperationID)
			if winnerErr == nil {
				matches := bytes.Equal(winner.RequestHash, hash[:]) && winner.UserID == request.UserID && winner.EndpointID == request.EndpointID && winner.Generation == int64(request.Generation) && winner.Role == "cli"
				if !matches {
					return ErrConflict
				}
				switch {
				case winner.State == "fulfilled":
					result, winnerErr = endpointRequestFromRow(winner)
					return winnerErr
				case winner.State == "pending" && winner.ExpiresAt.After(request.Now):
					result, winnerErr = endpointRequestFromRow(winner)
					return winnerErr
				case winner.State == "expired":
					renewed, renewErr := q.RenewExpiredCLIPeerEndpointEnrollmentRequest(ctx, dbsqlc.RenewExpiredCLIPeerEndpointEnrollmentRequestParams{CreatedAt: request.Now.UTC(), ExpiresAt: expiresAt.UTC(), OperationKey: request.OperationID, RequestHash: hash[:], UserID: request.UserID, EndpointID: request.EndpointID, Generation: int64(request.Generation), QuicPublicKey: request.QUICPublicKey[:]})
					if errors.Is(renewErr, sql.ErrNoRows) {
						return classifyCLIRenewalNoRows(ctx, q, request)
					}
					if renewErr != nil {
						return renewErr
					}
					result, renewErr = endpointRequestFromRow(renewed)
					if renewErr != nil {
						return renewErr
					}
					return r.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: request.UserID, ActorType: audit.ActorSystem, EventType: "peer_endpoint.cli_enrollment_renewed", ResourceType: "peer_endpoint_enrollment", ResourceID: renewed.ID, IdempotencyKey: request.OperationID + ":renew", Metadata: map[string]any{"endpoint_id": request.EndpointID, "generation": request.Generation, "role": "cli"}})
				default:
					return ErrConflict
				}
			}
			if !errors.Is(winnerErr, sql.ErrNoRows) {
				return winnerErr
			}
			_, pendingErr := q.GetMatchingPeerEndpointEnrollmentRequestForUpdate(ctx, dbsqlc.GetMatchingPeerEndpointEnrollmentRequestForUpdateParams{
				UserID: request.UserID, EndpointID: request.EndpointID, Generation: int64(request.Generation), Now: request.Now.UTC(),
			})
			if pendingErr == nil {
				return ErrConflict
			}
			if !errors.Is(pendingErr, sql.ErrNoRows) {
				return pendingErr
			}
			return ErrUnavailable
		}
		if err != nil {
			return err
		}
		result, err = endpointRequestFromRow(row)
		if err != nil {
			return err
		}
		return r.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: request.UserID, ActorType: audit.ActorSystem, EventType: "peer_endpoint.cli_enrollment_requested", ResourceType: "peer_endpoint_enrollment", ResourceID: row.ID, IdempotencyKey: request.OperationID, Metadata: map[string]any{"endpoint_id": request.EndpointID, "generation": request.Generation, "role": "cli"}})
	})
	return result, err
}

func requireActiveAccountRoot(ctx context.Context, q *dbsqlc.Queries, userID string) error {
	root, err := q.GetAccountE2EERootForUpdate(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnavailable
	}
	if err != nil {
		return err
	}
	if root.RevokedAt.Valid || root.Generation != 1 {
		return ErrUnavailable
	}
	keys, err := q.ListActiveAccountE2EEKeys(ctx, userID)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return ErrUnavailable
	}
	return nil
}

func classifyCLIRenewalNoRows(ctx context.Context, q *dbsqlc.Queries, request CLIEndpointRequest) error {
	_, err := q.GetMatchingPeerEndpointEnrollmentRequestForUpdate(ctx, dbsqlc.GetMatchingPeerEndpointEnrollmentRequestForUpdateParams{
		UserID: request.UserID, EndpointID: request.EndpointID, Generation: int64(request.Generation), Now: request.Now.UTC(),
	})
	if err == nil {
		return ErrConflict
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnavailable
	}
	return err
}

func (r *SQLRepository) ListPendingEndpoints(ctx context.Context, userID string, now time.Time, limit int32) ([]EndpointEnrollmentRequest, error) {
	if r == nil || ctx == nil || !wire.ValidIdentifier(userID) || now.IsZero() || limit < 1 || limit > 100 {
		return nil, wire.ErrInvalid
	}
	rows, err := r.store.Queries().ListPendingPeerEndpointEnrollmentRequests(ctx, dbsqlc.ListPendingPeerEndpointEnrollmentRequestsParams{UserID: userID, Now: now.UTC(), RowLimit: limit})
	if err != nil {
		return nil, err
	}
	result := make([]EndpointEnrollmentRequest, 0, len(rows))
	for _, row := range rows {
		value, err := endpointRequestFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}

func (r *SQLRepository) Register(ctx context.Context, operationID, userID string, proposed wire.Certificate, now time.Time) (wire.Certificate, error) {
	if r == nil || ctx == nil || now.IsZero() || len(operationID) < 16 || len(operationID) > 256 ||
		!wire.ValidIdentifier(userID) || proposed.AccountID != userID || len(proposed.Raw) == 0 {
		return wire.Certificate{}, wire.ErrInvalid
	}
	var result wire.Certificate
	err := r.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		q := tx.Queries()
		root, err := q.GetAccountE2EERootForUpdate(ctx, userID)
		if errors.Is(err, sql.ErrNoRows) || err == nil && root.RevokedAt.Valid {
			return ErrUnavailable
		}
		if err != nil {
			return err
		}
		key, err := q.GetAccountE2EEKeyByFingerprintForUpdate(ctx, dbsqlc.GetAccountE2EEKeyByFingerprintForUpdateParams{UserID: userID, Fingerprint: proposed.RootFingerprint[:]})
		if errors.Is(err, sql.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if key.RevokedAt.Valid || len(key.PublicKey) != ed25519.PublicKeySize || len(key.Fingerprint) != sha256.Size || key.KeyID != proposed.KeyID {
			return ErrConflict
		}
		result, err = r.registerTx(ctx, tx, operationID, userID, proposed, key.PublicKey, true, now)
		return err
	})
	return result, err
}

// RegisterMachine commits the key proven by the machine-control credential and
// the endpoint's self-signature in the same transaction as the certificate.
// No other endpoint signs or approves this machine's transport identity.
func (r *SQLRepository) RegisterMachine(ctx context.Context, request MachineRegistrationRequest, proposed wire.Certificate) (wire.Certificate, error) {
	if r == nil || ctx == nil || proposed.AccountID != request.UserID || proposed.EndpointID != request.MachineID ||
		proposed.Role != wire.RoleMachine || proposed.Generation != request.Generation ||
		proposed.Fingerprint != sha256.Sum256(request.Certificate) || !bytes.Equal(proposed.Raw, request.Certificate) {
		return wire.Certificate{}, wire.ErrInvalid
	}
	var result wire.Certificate
	err := r.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := tx.Queries().GetMachineForPeerEnrollment(ctx, dbsqlc.GetMachineForPeerEnrollmentParams{
			MachineID: request.MachineID, UserID: request.UserID, InstallationGeneration: int64(request.Generation),
		})
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnavailable
		}
		if err != nil {
			return err
		}
		result, err = r.registerTx(ctx, tx, request.OperationID, request.UserID, proposed, ed25519.PublicKey(proposed.QUICPublicKey[:]), false, request.Now)
		return err
	})
	return result, err
}

func (r *SQLRepository) registerTx(ctx context.Context, tx *db.Tx, operationID, userID string, proposed wire.Certificate, signer ed25519.PublicKey, requireEnrollment bool, now time.Time) (wire.Certificate, error) {
	if len(signer) != ed25519.PublicKeySize || proposed.KeyID != keyIDForFingerprint(sha256.Sum256(signer)) {
		return wire.Certificate{}, wire.ErrIdentity
	}
	q := tx.Queries()
	requestHash := sha256.Sum256(proposed.Raw)
	var result wire.Certificate
	existingOperation, err := q.GetPeerEndpointCertificateOperationForUpdate(ctx, operationID)
	if err == nil {
		if existingOperation.UserID != userID || !bytes.Equal(existingOperation.RequestHash, requestHash[:]) || !bytes.Equal(existingOperation.CertificateFingerprint, proposed.Fingerprint[:]) {
			return wire.Certificate{}, ErrConflict
		}
		row, getErr := q.GetPeerEndpointCertificateByFingerprint(ctx, existingOperation.CertificateFingerprint)
		if getErr != nil {
			return wire.Certificate{}, getErr
		}
		return certificateFromRow(row, now)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return wire.Certificate{}, err
	}
	// The signed not-before time includes clock-skew allowance. Enrollment
	// freshness and lifecycle timestamps belong to the server request clock,
	// never to that deliberately backdated certificate field.
	var enrollment dbsqlc.PeerEndpointEnrollmentRequest
	if requireEnrollment && (proposed.Role == wire.RoleMachine || proposed.Role == wire.RoleCLI) {
		enrollment, err = q.GetMatchingPeerEndpointEnrollmentRequestForUpdate(ctx, dbsqlc.GetMatchingPeerEndpointEnrollmentRequestForUpdateParams{UserID: userID, EndpointID: proposed.EndpointID, Generation: int64(proposed.Generation), Now: now})
		if errors.Is(err, sql.ErrNoRows) {
			return wire.Certificate{}, ErrUnavailable
		}
		if err != nil {
			return wire.Certificate{}, err
		}
		if roleFromString(enrollment.Role) != proposed.Role || !bytes.Equal(enrollment.QuicPublicKey, proposed.QUICPublicKey[:]) || now.Before(enrollment.CreatedAt) {
			return wire.Certificate{}, ErrConflict
		}
	}
	if _, err := q.RevokeSupersededPeerEndpointCertificates(ctx, dbsqlc.RevokeSupersededPeerEndpointCertificatesParams{Now: sql.NullTime{Time: now, Valid: true}, UserID: userID, EndpointID: proposed.EndpointID, Generation: int64(proposed.Generation), Serial: int64(proposed.Serial)}); err != nil {
		return wire.Certificate{}, err
	}
	row, err := q.CreatePeerEndpointCertificate(ctx, dbsqlc.CreatePeerEndpointCertificateParams{
		Fingerprint: proposed.Fingerprint[:], UserID: userID, KeyID: proposed.KeyID, EndpointID: proposed.EndpointID,
		Role: proposed.Role.String(), Generation: int64(proposed.Generation), Serial: int64(proposed.Serial),
		Certificate: proposed.Raw, QuicPublicKey: proposed.QUICPublicKey[:],
		SignerPublicKey: signer,
		IssuedAt:        proposed.IssuedAt, ExpiresAt: proposed.ExpiresAt,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return wire.Certificate{}, ErrConflict
	}
	if err != nil {
		return wire.Certificate{}, err
	}
	if _, err := q.CreatePeerEndpointCertificateOperation(ctx, dbsqlc.CreatePeerEndpointCertificateOperationParams{OperationID: operationID, UserID: userID, RequestHash: requestHash[:], CertificateFingerprint: proposed.Fingerprint[:], CreatedAt: now}); err != nil {
		return wire.Certificate{}, err
	}
	if requireEnrollment && (proposed.Role == wire.RoleMachine || proposed.Role == wire.RoleCLI) {
		if _, err := q.FulfillPeerEndpointEnrollmentRequest(ctx, dbsqlc.FulfillPeerEndpointEnrollmentRequestParams{CertificateFingerprint: proposed.Fingerprint[:], Now: sql.NullTime{Time: now, Valid: true}, ID: enrollment.ID}); err != nil {
			return wire.Certificate{}, err
		}
	}
	result, err = certificateFromRow(row, now)
	if err != nil {
		return wire.Certificate{}, err
	}
	if err := r.audit.WriteTx(ctx, tx, audit.Event{
		ActorUserID: userID, ActorType: audit.ActorUser, EventType: "peer_endpoint.certificate_registered",
		ResourceType: "peer_endpoint_certificate", ResourceID: hex.EncodeToString(proposed.Fingerprint[:]),
		IdempotencyKey: operationID, Metadata: map[string]any{"endpoint_id": proposed.EndpointID, "role": proposed.Role.String(), "generation": proposed.Generation, "serial": proposed.Serial},
	}); err != nil {
		return wire.Certificate{}, err
	}
	return result, nil
}

func endpointRequestFromRow(row dbsqlc.PeerEndpointEnrollmentRequest) (EndpointEnrollmentRequest, error) {
	if len(row.QuicPublicKey) != 32 || row.Generation < 1 || !row.ExpiresAt.After(row.CreatedAt) {
		return EndpointEnrollmentRequest{}, ErrUnavailable
	}
	result := EndpointEnrollmentRequest{ID: row.ID, UserID: row.UserID, EndpointID: row.EndpointID, Generation: uint64(row.Generation), Role: roleFromString(row.Role), State: row.State, CreatedAt: row.CreatedAt.UTC(), ExpiresAt: row.ExpiresAt.UTC()}
	if result.Role == 0 || (result.State != "pending" && result.State != "fulfilled" && result.State != "expired" && result.State != "revoked") {
		return EndpointEnrollmentRequest{}, ErrUnavailable
	}
	// CLI identities are intentionally single-generation. Reject malformed
	// rows rather than exposing a request that cannot produce the certificate
	// contract enforced by Bootstrap (role=cli, generation=1).
	if result.Role == wire.RoleCLI && result.Generation != 1 {
		return EndpointEnrollmentRequest{}, ErrUnavailable
	}
	copy(result.QUICPublicKey[:], row.QuicPublicKey)
	return result, nil
}

func (r *SQLRepository) Get(ctx context.Context, userID, endpointID string, generation uint64, now time.Time) (wire.Certificate, error) {
	if r == nil || ctx == nil || !wire.ValidIdentifier(userID) || !wire.ValidIdentifier(endpointID) || generation == 0 || generation > wire.MaximumInteger || now.IsZero() {
		return wire.Certificate{}, wire.ErrInvalid
	}
	var result wire.Certificate
	err := r.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		row, err := tx.Queries().GetActivePeerEndpointCertificateForUpdate(ctx, dbsqlc.GetActivePeerEndpointCertificateForUpdateParams{UserID: userID, EndpointID: endpointID, Generation: int64(generation), Now: now.UTC()})
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnavailable
		}
		if err != nil {
			return err
		}
		result, err = certificateFromRow(row, now)
		return err
	})
	return result, err
}

func (r *SQLRepository) Revoke(ctx context.Context, operationID, userID, endpointID string, generation, serial uint64, reason string, now time.Time) (wire.Certificate, error) {
	if r == nil || ctx == nil {
		return wire.Certificate{}, wire.ErrInvalid
	}
	var result wire.Certificate
	err := r.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		q := tx.Queries()
		replay, err := q.GetPeerEndpointCertificateRevocationForUpdate(ctx, operationID)
		if err == nil {
			if replay.UserID != userID || replay.Serial != int64(serial) || replay.Reason != reason {
				return ErrConflict
			}
			row, getErr := q.GetPeerEndpointCertificateByFingerprint(ctx, replay.CertificateFingerprint)
			if getErr != nil || row.EndpointID != endpointID || row.Generation != int64(generation) {
				return ErrConflict
			}
			result, err = certificateFromRow(row, row.IssuedAt)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		row, err := q.GetPeerEndpointCertificateByIdentity(ctx, dbsqlc.GetPeerEndpointCertificateByIdentityParams{UserID: userID, EndpointID: endpointID, Generation: int64(generation), Serial: int64(serial)})
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnavailable
		}
		if err != nil || row.Serial != int64(serial) || row.RevokedAt.Valid {
			if err != nil {
				return err
			}
			return ErrConflict
		}
		if _, err := q.CreatePeerEndpointCertificateRevocation(ctx, dbsqlc.CreatePeerEndpointCertificateRevocationParams{OperationID: operationID, UserID: userID, CertificateFingerprint: row.Fingerprint, Serial: int64(serial), Reason: reason, CreatedAt: now}); err != nil {
			return err
		}
		row, err = q.RevokePeerEndpointCertificate(ctx, dbsqlc.RevokePeerEndpointCertificateParams{Now: sql.NullTime{Time: now, Valid: true}, Reason: sql.NullString{String: reason, Valid: true}, Fingerprint: row.Fingerprint})
		if err != nil {
			return err
		}
		result, err = certificateFromRow(row, row.IssuedAt)
		if err != nil {
			return err
		}
		return r.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "peer_endpoint.certificate_revoked", ResourceType: "peer_endpoint_certificate", ResourceID: hex.EncodeToString(row.Fingerprint), IdempotencyKey: operationID, Metadata: map[string]any{"endpoint_id": endpointID, "generation": generation, "serial": serial, "reason": reason}})
	})
	return result, err
}

func certificateFromRow(row dbsqlc.PeerEndpointCertificate, now time.Time) (wire.Certificate, error) {
	if len(row.SignerPublicKey) != ed25519.PublicKeySize {
		return wire.Certificate{}, ErrUnavailable
	}
	keyFingerprint := sha256.Sum256(row.SignerPublicKey)
	if row.KeyID != keyIDForFingerprint(keyFingerprint) {
		return wire.Certificate{}, ErrConflict
	}
	certificate, err := wire.Verify(row.Certificate, row.SignerPublicKey, wire.Expected{AccountID: row.UserID, EndpointID: row.EndpointID, Role: roleFromString(row.Role), Generation: uint64(row.Generation), Serial: uint64(row.Serial)}, now)
	if err != nil {
		return wire.Certificate{}, err
	}
	if !bytes.Equal(row.Fingerprint, certificate.Fingerprint[:]) || !bytes.Equal(row.QuicPublicKey, certificate.QUICPublicKey[:]) || !row.IssuedAt.Equal(certificate.IssuedAt) || !row.ExpiresAt.Equal(certificate.ExpiresAt) {
		return wire.Certificate{}, ErrConflict
	}
	certificate.KeyID = row.KeyID
	certificate.RootFingerprint = keyFingerprint
	return certificate, nil
}

func accountKeyFromRow(row dbsqlc.AccountE2eeKey) (AccountKey, error) {
	if len(row.PublicKey) != ed25519.PublicKeySize || len(row.Fingerprint) != sha256.Size || row.KeyID == "" || row.Generation <= 0 || row.RevokedAt.Valid {
		return AccountKey{}, ErrUnavailable
	}
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], row.Fingerprint)
	if fingerprint != sha256.Sum256(row.PublicKey) || row.KeyID != keyIDForFingerprint(fingerprint) {
		return AccountKey{}, ErrConflict
	}
	return AccountKey{KeyID: row.KeyID, PublicKey: append(ed25519.PublicKey(nil), row.PublicKey...), Fingerprint: fingerprint, Generation: uint64(row.Generation)}, nil
}

func roleFromString(value string) wire.Role {
	switch value {
	case "cli":
		return wire.RoleCLI
	case "machine":
		return wire.RoleMachine
	default:
		return 0
	}
}
