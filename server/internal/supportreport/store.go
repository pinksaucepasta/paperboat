package supportreport

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
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
	// One statement: the occurrence row (narrow, so every shown reference resolves) and, only when it was stored, its
	// group (one row per account and fingerprint: first/last seen, count, the fault's detail from the newest report).
	_, err = tx.Exec(ctx, `WITH occ AS (
INSERT INTO support_report_occurrences (id,owner_id,support_reference,fingerprint,request_id,correlation_id,release,created_at,expires_at,sentry_event_id,reporting)
SELECT $1,$3,$2,paperboat.support_fingerprint($5,$16,$7,$8,$18,$19,$22,$23,$12,$13,$14),$4,$17,$6,$9,$10,$11,$15
WHERE (SELECT count(*) FROM support_report_occurrences WHERE owner_id=$3 AND support_reference=$2)<32 ON CONFLICT DO NOTHING
RETURNING fingerprint)
INSERT INTO support_report_groups (owner_id,fingerprint,component,operation,stage,code,cause,error_type,http_status,outcome,source_file,source_function,source_line,severity,errno,error_chain,first_seen,last_seen,occurrences,expires_at)
SELECT $3,occ.fingerprint,$5,$16,$7,$8,$18,$19,$22,$23,$12,$13,$14,$24,$21,coalesce($20::text[],'{}'),$9,$9,1,$10 FROM occ
ON CONFLICT (owner_id,fingerprint) DO UPDATE SET occurrences=support_report_groups.occurrences+1,
first_seen=LEAST(support_report_groups.first_seen,EXCLUDED.first_seen),last_seen=GREATEST(support_report_groups.last_seen,EXCLUDED.last_seen),
expires_at=GREATEST(support_report_groups.expires_at,EXCLUDED.expires_at),severity=EXCLUDED.severity,errno=EXCLUDED.errno,error_chain=EXCLUDED.error_chain`,
		r.ID, r.Reference, r.OwnerID, r.RequestID, r.Component, r.Release, r.Stage, r.Code, r.At, r.ExpiresAt, r.EventID, r.SourceFile, r.SourceFunction, r.SourceLine, r.Reporting, r.Operation, r.CorrelationID, r.Cause, r.ErrorType, r.ErrorChain, r.Errno, r.HTTPStatus, r.Outcome, r.Severity)
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
		rows, err := tx.Query(ctx, `SELECT o.id,o.support_reference,o.owner_id,o.request_id,g.component,o.release,g.stage,g.code,o.created_at,o.expires_at,o.sentry_event_id,g.source_file,g.source_function,g.source_line,o.reporting,g.operation,o.correlation_id,g.cause,g.error_type,g.error_chain,g.errno,g.http_status,g.outcome,g.severity,g.first_seen,g.last_seen,g.occurrences FROM support_report_occurrences o JOIN support_report_groups g ON g.owner_id=o.owner_id AND g.fingerprint=o.fingerprint WHERE (o.owner_id=$1 OR $3) AND o.support_reference=$2 AND o.expires_at>now() ORDER BY o.created_at,o.id LIMIT 129`, owner, ref, operator && owner == "")
		if err != nil {
			return err
		}
		defer rows.Close()
		records = records[:0]
		for rows.Next() {
			var r Record
			if err := rows.Scan(&r.ID, &r.Reference, &r.OwnerID, &r.RequestID, &r.Component, &r.Release, &r.Stage, &r.Code, &r.At, &r.ExpiresAt, &r.EventID, &r.SourceFile, &r.SourceFunction, &r.SourceLine, &r.Reporting, &r.Operation, &r.CorrelationID, &r.Cause, &r.ErrorType, &r.ErrorChain, &r.Errno, &r.HTTPStatus, &r.Outcome, &r.Severity, &r.GroupFirstSeen, &r.GroupLastSeen, &r.GroupOccurrences); err != nil {
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
	// Occurrences first: a group outlives every occurrence it explains (its expires_at is the newest one's).
	if _, err := s.db.Pool().Exec(ctx, `DELETE FROM support_report_occurrences WHERE id IN (SELECT id FROM support_report_occurrences WHERE expires_at<=now() ORDER BY expires_at LIMIT 1000)`); err != nil {
		return err
	}
	_, err := s.db.Pool().Exec(ctx, `DELETE FROM support_report_groups WHERE (owner_id,fingerprint) IN (SELECT owner_id,fingerprint FROM support_report_groups WHERE expires_at<=now() ORDER BY expires_at LIMIT 1000)`)
	return err
}
