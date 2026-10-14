package supportreport

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

type SQLStore struct {
	db    *db.DB
	audit *audit.Writer
}

func NewSQLStore(database *db.DB) (*SQLStore, error) {
	if database == nil || database.Pool() == nil {
		return nil, ErrInvalid
	}
	return &SQLStore{database, audit.NewWriter(database)}, nil
}
func (s *SQLStore) Save(ctx context.Context, r Record) error {
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Serialize the per-operation cap across server replicas. A later retry can
	// never replace the first captured failure.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, r.OwnerID+":"+r.Reference); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO support_reports (
id,support_reference,owner_id,request_id,component,release,stage,code,created_at,expires_at,sentry_event_id,source_file,source_function,source_line,reporting,
operation,correlation_id,cause,error_type,error_chain,errno,http_status,outcome,severity
) SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24
WHERE (SELECT count(*) FROM support_reports WHERE owner_id=$3 AND support_reference=$2)<32 ON CONFLICT DO NOTHING`, r.ID, r.Reference, r.OwnerID, r.RequestID, r.Component, r.Release, r.Stage, r.Code, r.At, r.ExpiresAt, r.EventID, r.SourceFile, r.SourceFunction, r.SourceLine, r.Reporting, r.Operation, r.CorrelationID, r.Cause, r.ErrorType, r.ErrorChain, r.Errno, r.HTTPStatus, r.Outcome, r.Severity)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *SQLStore) Lookup(ctx context.Context, actor, owner, ref string, operator bool) ([]Record, error) {
	records := []Record{}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		// Recheck current global authority inside the audited read transaction.
		if operator {
			var allowed bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_accounts WHERE id=$1 AND status='active' AND role IN ('admin','support','billing'))`, actor).Scan(&allowed); err != nil {
				return err
			}
			if !allowed {
				return errors.New("operator authority required")
			}
		} else if actor != owner {
			return ErrInvalid
		}
		eventType := "support.self_lookup"
		actorType := audit.ActorUser
		if operator {
			eventType = "support.operator_lookup"
			actorType = audit.ActorAdmin
		}

		// Use the same transaction for lookup and audit.
		rows, err := tx.Query(ctx, `SELECT id,support_reference,owner_id,request_id,component,release,stage,code,created_at,expires_at,sentry_event_id,source_file,source_function,source_line,reporting,operation,correlation_id,cause,error_type,error_chain,errno,http_status,outcome,severity FROM support_reports WHERE (owner_id=$1 OR $3) AND support_reference=$2 AND expires_at>now() ORDER BY created_at,id LIMIT 129`, owner, ref, operator && owner == "")
		if err != nil {
			return err
		}
		defer rows.Close()
		records = records[:0]
		for rows.Next() {
			var r Record
			if err := rows.Scan(&r.ID, &r.Reference, &r.OwnerID, &r.RequestID, &r.Component, &r.Release, &r.Stage, &r.Code, &r.At, &r.ExpiresAt, &r.EventID, &r.SourceFile, &r.SourceFunction, &r.SourceLine, &r.Reporting, &r.Operation, &r.CorrelationID, &r.Cause, &r.ErrorType, &r.ErrorChain, &r.Errno, &r.HTTPStatus, &r.Outcome, &r.Severity); err != nil {
				return err
			}
			records = append(records, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		outcome := "found"
		if len(records) == 0 {
			outcome = "unavailable"
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: actorType, EventType: eventType, ResourceType: "support_reference", ResourceID: ref, IdempotencyKey: NewReference(), Metadata: map[string]any{"owner_id": owner, "outcome": outcome}})
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}
func (s *SQLStore) Cleanup(ctx context.Context) error {
	_, err := s.db.Pool().Exec(ctx, `DELETE FROM support_reports WHERE id IN (SELECT id FROM support_reports WHERE expires_at<=now() ORDER BY expires_at LIMIT 1000)`)
	return err
}
