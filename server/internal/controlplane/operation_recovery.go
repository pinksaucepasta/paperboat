package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

var (
	ErrRecoveryKeyConflict        = errors.New("operation recovery key conflict")
	ErrOperationNotDeadLettered   = errors.New("operation is not dead-lettered")
	ErrOperationRecoveryForbidden = errors.New("operation recovery permission denied")
	ErrAdminMutationForbidden     = errors.New("admin mutation permission denied")
)

type OperationRecoveryService struct {
	store *db.DB
	audit *audit.Writer
}

func NewOperationRecoveryService(store *db.DB, writer *audit.Writer) *OperationRecoveryService {
	if writer == nil && store != nil {
		writer = audit.NewWriter(store)
	}
	return &OperationRecoveryService{store: store, audit: writer}
}

func (s *OperationRecoveryService) Recover(ctx context.Context, actorID, recoveryKey, operationID string, now time.Time) error {
	recoveryKey = strings.TrimSpace(recoveryKey)
	operationID = strings.TrimSpace(operationID)
	if recoveryKey == "" || operationID == "" || len(recoveryKey) > 256 {
		return fmt.Errorf("recovery key and operation id are required")
	}
	return s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := requireActiveGlobalRole(ctx, tx, actorID, "operations", "admin"); err != nil {
			if errors.Is(err, ErrAdminMutationForbidden) {
				return ErrOperationRecoveryForbidden
			}
			return err
		}
		_, err := tx.Queries().ReserveControlOperationRecovery(ctx, dbsqlc.ReserveControlOperationRecoveryParams{OperationKey: recoveryKey, OperationID: operationID, ActorUserID: sql.NullString{String: actorID, Valid: actorID != ""}})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			existing, getErr := tx.Queries().GetControlOperationRecovery(ctx, recoveryKey)
			if getErr != nil {
				return getErr
			}
			if existing.OperationID != operationID {
				return ErrRecoveryKeyConflict
			}
			return nil
		}
		updated, err := tx.Queries().RecoverDeadLetterControlOperation(ctx, dbsqlc.RecoverDeadLetterControlOperationParams{ID: operationID, Now: now.UTC()})
		if err != nil {
			return err
		}
		if updated == 0 {
			return ErrOperationNotDeadLettered
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actorID, ActorType: audit.ActorAdmin, EventType: "control.operation_recovered", ResourceType: "control_operation", ResourceID: operationID, IdempotencyKey: recoveryKey})
	})
}

func requireActiveGlobalRole(ctx context.Context, tx *db.Tx, actorID string, roles ...string) error {
	var role string
	if err := tx.QueryRow(ctx, `SELECT role FROM operator_accounts WHERE id=$1 AND status='active'`, actorID).Scan(&role); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			return ErrAdminMutationForbidden
		}
		return err
	}
	for _, allowed := range roles {
		if role == allowed {
			return nil
		}
	}
	return ErrAdminMutationForbidden
}
