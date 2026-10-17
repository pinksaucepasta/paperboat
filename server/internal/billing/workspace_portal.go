package billing

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/secrets"
)

type portalProviderRequest struct{ CustomerID, ReturnURL, ConfigurationID string }
type preparedPortal struct {
	request portalProviderRequest
	replay  *CustomerPortalSession
}

func (s *WorkspaceService) Portal(ctx context.Context, account, team, key, returnURL string) (CustomerPortalSession, error) {
	if s.stripe == nil {
		return CustomerPortalSession{}, ErrStripeUnavailable
	}
	if s.key == "" {
		return CustomerPortalSession{}, ErrWorkspaceEncryptionUnavailable
	}
	hash, err := workspaceRequestHash(struct{ Actor, Team, Return string }{account, team, returnURL})
	if err != nil {
		return CustomerPortalSession{}, err
	}
	prepared, err := s.preparePortal(ctx, account, team, key, returnURL, hash)
	if err != nil {
		return CustomerPortalSession{}, err
	}
	if prepared.replay != nil {
		return *prepared.replay, nil
	}
	if _, err = s.store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspace_operations SET provider_attempted_at=coalesce(provider_attempted_at,now()) WHERE idempotency_key=$1 AND state='pending'`, key); err != nil {
		return CustomerPortalSession{}, err
	}
	request := prepared.request
	result, err := s.stripe.CreatePortal(ctx, StripePortalInput{CustomerID: request.CustomerID, ReturnURL: request.ReturnURL, ConfigurationID: request.ConfigurationID, IdempotencyKey: key})
	if err != nil {
		return CustomerPortalSession{}, s.failOperation(ctx, key, err)
	}
	if err = s.completeOperation(ctx, key, result); err != nil {
		_ = s.markOperation(ctx, key, "uncertain")
		return CustomerPortalSession{}, errors.Join(ErrProviderOutcomeUnknown, err)
	}
	if err = s.reauthorizeBilling(ctx, account, team); err != nil {
		return CustomerPortalSession{}, err
	}
	return result, nil
}

func (s *WorkspaceService) preparePortal(ctx context.Context, account, team, key, returnURL, hash string) (preparedPortal, error) {
	var out preparedPortal
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		w, err := WorkspaceTx(ctx, tx, account, team, true)
		if err != nil {
			return err
		}
		if w.CustomerID == "" {
			return errors.New("workspace has no billing customer")
		}
		configuration := s.portals.Personal
		if w.TeamID != "" {
			configuration = s.portals.Team
		}
		out.request = portalProviderRequest{w.CustomerID, returnURL, configuration}
		op, found, err := loadWorkspaceOperation(ctx, tx, key)
		if err != nil {
			return err
		}
		if found {
			if op.WorkspaceID != w.ID || op.Actor != account || op.Kind != "portal" || op.Hash != hash {
				return ErrIdempotencyConflict
			}
			switch op.State {
			case "succeeded":
				var result CustomerPortalSession
				if err = s.decryptResult(op.Ciphertext, &result); err != nil {
					return err
				}
				out.replay = &result
				return nil
			case "failed":
				return ErrWorkspaceOperationFailed
			case "uncertain", "pending":
				if op.State == "pending" && time.Since(op.UpdatedAt) < time.Minute {
					return ErrCheckoutPending
				}
				if op.ProviderAttemptedAt.Valid && time.Since(op.ProviderAttemptedAt.Time) >= 24*time.Hour {
					return ErrStripeOperationExpired
				}
				if err = s.decryptResult(op.RequestCiphertext, &out.request); err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `UPDATE billing_workspace_operations SET state='pending',updated_at=now() WHERE idempotency_key=$1 AND state=$2`, key, op.State)
				return err
			default:
				return ErrCheckoutPending
			}
		}
		if configuration == "" {
			return ErrStripeUnavailable
		}
		body, err := json.Marshal(out.request)
		if err != nil {
			return err
		}
		ciphertext, err := secrets.Encrypt(s.key, string(body))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state,request_ciphertext) VALUES($1,$2,$3,'portal',$4,'pending',$5)`, key, w.ID, account, hash, ciphertext)
		return err
	})
	return out, err
}
