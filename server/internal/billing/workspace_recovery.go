package billing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

var ErrWorkspaceRecoveryInvalid = errors.New("workspace billing recovery request is invalid")
var ErrWorkspaceRecoveryTooEarly = errors.New("workspace billing recovery requires an operation at least 24 hours old")

type WorkspaceRecoveryService struct {
	store   *db.DB
	audit   *audit.Writer
	billing *WorkspaceService
}

func NewWorkspaceRecoveryService(store *db.DB, writer *audit.Writer, billing *WorkspaceService) *WorkspaceRecoveryService {
	if writer == nil && store != nil {
		writer = audit.NewWriter(store)
	}
	return &WorkspaceRecoveryService{store: store, audit: writer, billing: billing}
}

// ReconcileCheckout verifies the immutable checkout request against Stripe. An
// open session restores its original URL, a complete session is reconciled from
// the authoritative subscription, and an expired session becomes terminal.
func (s *WorkspaceRecoveryService) ReconcileCheckout(ctx context.Context, actorID, recoveryKey, operationKey, providerSessionID, evidenceReference string) error {
	actorID, recoveryKey, operationKey, providerSessionID, evidenceReference = strings.TrimSpace(actorID), strings.TrimSpace(recoveryKey), strings.TrimSpace(operationKey), strings.TrimSpace(providerSessionID), strings.TrimSpace(evidenceReference)
	if s == nil || s.store == nil || s.audit == nil || s.billing == nil || actorID == "" || len(recoveryKey) < 8 || len(recoveryKey) > 256 || operationKey == "" || providerSessionID == "" || !evidenceReferencePattern.MatchString(evidenceReference) {
		return ErrWorkspaceRecoveryInvalid
	}
	if s.billing.stripe == nil {
		return ErrStripeUnavailable
	}
	var workspaceID string
	var requestCipher []byte
	replay := false
	receiptDisposition := ""
	if err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := requireRecoveryAdmin(ctx, tx, actorID); err != nil {
			return err
		}
		var receiptOp string
		var receiptHash []byte
		err := tx.QueryRow(ctx, `SELECT operation_key,disposition,request_hash FROM billing_workspace_recoveries WHERE idempotency_key=$1`, recoveryKey).Scan(&receiptOp, &receiptDisposition, &receiptHash)
		if err == nil {
			requestHash := recoveryRequestHash(actorID, operationKey, receiptDisposition, providerSessionID, evidenceReference)
			if receiptOp != operationKey || !bytes.Equal(receiptHash, requestHash[:]) {
				return ErrBillingRecoveryConflict
			}
			replay = true
		} else if err != pgx.ErrNoRows {
			return err
		}
		var kind, state string
		var attempted sql.NullTime
		if err := tx.QueryRow(ctx, `SELECT workspace_id,kind,state,request_ciphertext,provider_attempted_at FROM billing_workspace_operations WHERE idempotency_key=$1 FOR UPDATE`, operationKey).Scan(&workspaceID, &kind, &state, &requestCipher, &attempted); err != nil {
			if err == pgx.ErrNoRows {
				return ErrBillingRecoveryInvalid
			}
			return err
		}
		if kind != "checkout" || (!replay && state != "pending" && state != "uncertain") || (replay && state != "succeeded" && state != "failed") {
			return ErrBillingOperationNotUncertain
		}
		if !attempted.Valid || time.Since(attempted.Time) < 24*time.Hour {
			return ErrWorkspaceRecoveryTooEarly
		}
		return nil
	}); err != nil {
		return err
	}
	var request checkoutProviderRequest
	if err := s.billing.decryptResult(requestCipher, &request); err != nil {
		return err
	}
	state, err := s.billing.stripe.GetCheckoutSession(ctx, providerSessionID)
	if err != nil {
		return err
	}
	if state.ID != providerSessionID || state.CustomerID != request.CustomerID || state.CheckoutOperation != operationKey || state.Mode != "subscription" || state.PriceID != request.PriceID || state.Quantity != request.Quantity {
		return ErrWorkspaceRecoveryInvalid
	}
	disposition := "recovered_checkout"
	if state.Status == "expired" {
		disposition = "expired_checkout"
	} else if state.Status != "open" && state.Status != "complete" {
		return ErrWorkspaceRecoveryInvalid
	}
	if state.Status == "open" && state.URL == "" {
		return ErrWorkspaceRecoveryInvalid
	}
	if replay {
		disposition = receiptDisposition
	}
	if state.Status == "complete" && state.SubscriptionID == "" {
		return ErrWorkspaceRecoveryInvalid
	}
	result := CheckoutSession{URL: state.URL, ProviderSessionID: state.ID, ProviderCustomerID: state.CustomerID}
	resultCipher, err := s.billing.encryptResult(result)
	if err != nil {
		return err
	}
	if err = s.recordRecovery(ctx, actorID, recoveryKey, operationKey, workspaceID, disposition, providerSessionID, evidenceReference, func(tx *db.Tx) error {
		newState := "succeeded"
		if disposition == "expired_checkout" {
			newState = "failed"
			resultCipher = nil
		}
		tag, e := tx.Exec(ctx, `UPDATE billing_workspace_operations SET state=$2,result_ciphertext=$3,updated_at=now() WHERE idempotency_key=$1 AND state IN('pending','uncertain')`, operationKey, newState, resultCipher)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return ErrBillingOperationNotUncertain
		}
		return nil
	}); err != nil {
		return err
	}
	if state.Status == "complete" {
		_, err = s.billing.reconcileSubscription(ctx, state.SubscriptionID, nil)
	}
	return err
}

func (s *WorkspaceRecoveryService) SupersedePortal(ctx context.Context, actorID, recoveryKey, operationKey, evidenceReference string) error {
	return s.retire(ctx, actorID, recoveryKey, operationKey, evidenceReference, "superseded_portal", "portal")
}

// RetireNotApplied records provider evidence that an expired unknown checkout,
// portal, or seat operation produced no provider mutation. Applied outcomes must be
// reconciled from Stripe state; this method never fabricates a provider result.
func (s *WorkspaceRecoveryService) RetireNotApplied(ctx context.Context, actorID, recoveryKey, operationKey, evidenceReference string) error {
	return s.retire(ctx, actorID, recoveryKey, operationKey, evidenceReference, "not_applied", "")
}

func (s *WorkspaceRecoveryService) retire(ctx context.Context, actorID, recoveryKey, operationKey, evidenceReference, disposition, requiredKind string) error {
	actorID, recoveryKey, operationKey, evidenceReference = strings.TrimSpace(actorID), strings.TrimSpace(recoveryKey), strings.TrimSpace(operationKey), strings.TrimSpace(evidenceReference)
	if s == nil || s.store == nil || s.audit == nil || actorID == "" || len(recoveryKey) < 8 || len(recoveryKey) > 256 || operationKey == "" || len(operationKey) > 256 || !evidenceReferencePattern.MatchString(evidenceReference) {
		return ErrWorkspaceRecoveryInvalid
	}
	requestJSON, _ := json.Marshal([]string{actorID, operationKey, disposition, evidenceReference})
	requestHash := sha256.Sum256(requestJSON)
	evidenceHash := sha256.Sum256([]byte(evidenceReference))
	return s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := requireRecoveryAdmin(ctx, tx, actorID); err != nil {
			return err
		}
		var existingOperation, existingWorkspace string
		var existingHash []byte
		err := tx.QueryRow(ctx, `SELECT operation_key,workspace_id,request_hash FROM billing_workspace_recoveries WHERE idempotency_key=$1`, recoveryKey).Scan(&existingOperation, &existingWorkspace, &existingHash)
		if err == nil {
			if existingOperation != operationKey || !bytes.Equal(existingHash, requestHash[:]) {
				return ErrBillingRecoveryConflict
			}
			return nil
		}
		if err != pgx.ErrNoRows {
			return err
		}
		var workspaceID, kind, state string
		var attempted sql.NullTime
		err = tx.QueryRow(ctx, `SELECT workspace_id,kind,state,provider_attempted_at FROM billing_workspace_operations WHERE idempotency_key=$1 FOR UPDATE`, operationKey).Scan(&workspaceID, &kind, &state, &attempted)
		if err == pgx.ErrNoRows {
			return ErrBillingRecoveryInvalid
		}
		if err != nil {
			return err
		}
		if kind != "checkout" && kind != "portal" && kind != "seats" {
			return ErrBillingRecoveryInvalid
		}
		if requiredKind != "" && kind != requiredKind {
			return ErrBillingRecoveryInvalid
		}
		if state != "pending" && state != "uncertain" {
			return ErrBillingOperationNotUncertain
		}
		if !attempted.Valid || time.Since(attempted.Time) < 24*time.Hour {
			return ErrWorkspaceRecoveryTooEarly
		}
		tag, err := tx.Exec(ctx, `UPDATE billing_workspace_operations SET state='failed',updated_at=now() WHERE idempotency_key=$1 AND workspace_id=$2 AND state IN('pending','uncertain')`, operationKey, workspaceID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrBillingOperationNotUncertain
		}
		if _, err = tx.Exec(ctx, `INSERT INTO billing_workspace_recoveries(idempotency_key,operation_key,workspace_id,actor_user_id,disposition,request_hash,evidence_sha256) VALUES($1,$2,$3,$4,$5,$6,$7)`, recoveryKey, operationKey, workspaceID, actorID, disposition, requestHash[:], evidenceHash[:]); err != nil {
			return err
		}
		if kind == "seats" {
			if _, err = tx.Exec(ctx, `UPDATE billing_workspaces SET pending_seat_capacity=NULL,revision=revision+1,updated_at=now() WHERE id=$1`, workspaceID); err != nil {
				return err
			}
		}
		eventType := "billing.workspace_operation_not_applied"
		if disposition == "superseded_portal" {
			eventType = "billing.workspace_portal_superseded"
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actorID, ActorType: audit.ActorAdmin, EventType: eventType, ResourceType: "billing_workspace_operation", ResourceID: operationKey, IdempotencyKey: "workspace-recovery:" + recoveryKey, Metadata: map[string]any{"workspace_id": workspaceID, "operation_kind": kind, "disposition": disposition, "evidence_sha256": hex.EncodeToString(evidenceHash[:])}})
	})
}

func requireRecoveryAdmin(ctx context.Context, tx *db.Tx, actorID string) error {
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_accounts WHERE id=$1 AND status='active' AND role IN ('billing','admin'))`, actorID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrWorkspaceForbidden
	}
	return nil
}

func recoveryRequestHash(parts ...string) [32]byte {
	b, _ := json.Marshal(parts)
	return sha256.Sum256(b)
}
func (s *WorkspaceRecoveryService) recordRecovery(ctx context.Context, actorID, recoveryKey, operationKey, workspaceID, disposition, binding, evidence string, mutate func(*db.Tx) error) error {
	rh := recoveryRequestHash(actorID, operationKey, disposition, binding, evidence)
	eh := sha256.Sum256([]byte(evidence))
	return s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := requireRecoveryAdmin(ctx, tx, actorID); err != nil {
			return err
		}
		var op, ws string
		var old []byte
		err := tx.QueryRow(ctx, `SELECT operation_key,workspace_id,request_hash FROM billing_workspace_recoveries WHERE idempotency_key=$1`, recoveryKey).Scan(&op, &ws, &old)
		if err == nil {
			if op != operationKey || !bytes.Equal(old, rh[:]) {
				return ErrBillingRecoveryConflict
			}
			return nil
		}
		if err != pgx.ErrNoRows {
			return err
		}
		if err = mutate(tx); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO billing_workspace_recoveries(idempotency_key,operation_key,workspace_id,actor_user_id,disposition,request_hash,evidence_sha256) VALUES($1,$2,$3,$4,$5,$6,$7)`, recoveryKey, operationKey, workspaceID, actorID, disposition, rh[:], eh[:]); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actorID, ActorType: audit.ActorAdmin, EventType: "billing.workspace_operation_recovered", ResourceType: "billing_workspace_operation", ResourceID: operationKey, IdempotencyKey: "workspace-recovery:" + recoveryKey, Metadata: map[string]any{"workspace_id": workspaceID, "disposition": disposition, "evidence_sha256": hex.EncodeToString(eh[:])}})
	})
}
