package controlplane

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

var trk35PostgresFixtureCounter atomic.Uint64

// TestTRK35PostgresChaosTransactionRollbackAndConnectionRetry exercises the
// durable operation/audit boundary against PostgreSQL. The first transaction
// is intentionally rolled back; the second loses its live connection before
// commit. Retrying the same operation and audit key must leave one coherent
// operation, one audit event, and no partial row from either failed attempt.
func TestTRK35PostgresChaosTransactionRollbackAndConnectionRetry(t *testing.T) {
	database, fixture := newTRK35PostgresFixture(t)
	ctx := context.Background()

	requestHash := sha256.Sum256([]byte("trk35-request-" + fixture.suffix))
	operationID := "op_trk35_" + fixture.suffix
	idempotencyKey := "trk35-idem-" + fixture.suffix
	correlationID := "corr_trk35_" + fixture.suffix
	auditID := "aud_trk35_" + fixture.suffix
	eventType := "trk35.transaction.committed"
	resourceType := "tunnel"
	resourceID := fixture.tunnelID

	rollbackErr := errors.New("trk35 intentional transaction interruption")
	err := database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := trk35InsertOperation(ctx, tx, operationID, fixture.accountID, idempotencyKey, requestHash[:], resourceID, correlationID); err != nil {
			return err
		}
		if err := trk35InsertAudit(ctx, tx, auditID, fixture.accountID, eventType, resourceType, resourceID, idempotencyKey, correlationID); err != nil {
			return err
		}
		return rollbackErr
	})
	if !errors.Is(err, rollbackErr) {
		t.Fatalf("interrupted transaction error = %v, want %v", err, rollbackErr)
	}
	trk35AssertOperationAndAuditCount(t, database, operationID, auditID, 0, 0)

	// A closed pgx connection is the deterministic equivalent of a process or
	// network failure during an in-flight commit. The server rolls back the
	// transaction; the caller cannot know the commit result and must retry the
	// same idempotency key.
	conn, err := database.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		t.Fatal(err)
	}
	if err := trk35InsertOperationPGX(ctx, tx, operationID, fixture.accountID, idempotencyKey, requestHash[:], resourceID, correlationID); err != nil {
		_ = tx.Rollback(ctx)
		conn.Release()
		t.Fatal(err)
	}
	if err := trk35InsertAuditPGX(ctx, tx, auditID, fixture.accountID, eventType, resourceType, resourceID, idempotencyKey, correlationID); err != nil {
		_ = tx.Rollback(ctx)
		conn.Release()
		t.Fatal(err)
	}
	if err := conn.Conn().Close(context.Background()); err != nil {
		conn.Release()
		t.Fatalf("close transaction connection: %v", err)
	}
	if err := tx.Commit(context.Background()); err == nil {
		conn.Release()
		t.Fatal("commit unexpectedly succeeded after the transaction connection was closed")
	}
	conn.Release()
	trk35AssertOperationAndAuditCount(t, database, operationID, auditID, 0, 0)

	// The retry uses the production DB transaction wrapper and conflict-safe
	// writes. Run it twice to model both a rolled-back attempt and an unknown
	// commit outcome observed by the caller.
	for attempt := 0; attempt < 2; attempt++ {
		err := database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			if err := trk35InsertOperationIdempotent(ctx, tx, operationID, fixture.accountID, idempotencyKey, requestHash[:], resourceID, correlationID); err != nil {
				return err
			}
			return trk35InsertAudit(ctx, tx, auditID, fixture.accountID, eventType, resourceType, resourceID, idempotencyKey, correlationID)
		})
		if err != nil {
			t.Fatalf("idempotent retry %d: %v", attempt+1, err)
		}
	}
	trk35AssertOperationAndAuditCount(t, database, operationID, auditID, 1, 1)

	var storedCorrelation, auditCorrelation string
	if err := database.SQL().QueryRowContext(ctx, `
SELECT o.correlation_id, ae.correlation_id
FROM paperboat.operations AS o
JOIN paperboat.audit_events AS ae
  ON ae.event_type = $2
 AND ae.resource_type = $3
 AND ae.resource_id = $4
 AND ae.idempotency_key = $5
WHERE o.id = $1`, operationID, eventType, resourceType, resourceID, idempotencyKey).Scan(&storedCorrelation, &auditCorrelation); err != nil {
		t.Fatal(err)
	}
	if storedCorrelation != correlationID || auditCorrelation != correlationID {
		t.Fatalf("operation/audit correlation = %q/%q, want %q", storedCorrelation, auditCorrelation, correlationID)
	}
}

func newTRK35PostgresFixture(t *testing.T) (*db.DB, tunnelEdgeAcceptanceFixture) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run TRK-35 PostgreSQL chaos acceptance")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.Migrate(ctx, database); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d-%d", time.Now().UnixNano(), trk35PostgresFixtureCounter.Add(1))
	now := time.Now().UTC().Truncate(time.Microsecond)
	fixture := newTunnelEdgeAcceptanceFixture(suffix, now)
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		// Audit events are append-only by contract, so the exact isolated
		// database created by the acceptance harness is the cleanup boundary for
		// these two retained correlation records.
		if _, err := database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.audit_events WHERE account_id=$1`, fixture.accountID); err != nil {
			t.Logf("audit events retained by append-only contract: %v", err)
		}
		_, _ = database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.operations WHERE account_id=$1`, fixture.accountID)
		_, _ = database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.users WHERE id=$1`, fixture.accountID)
		_, _ = database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, fixture.nodeID)
		_ = database.Close()
	})
	fixture.insertBase(t, database)
	return database, fixture
}

func trk35InsertOperation(ctx context.Context, tx *db.Tx, operationID, accountID, idempotencyKey string, requestHash []byte, resourceID, correlationID string) error {
	_, err := tx.Exec(ctx, `
INSERT INTO operations
  (id, account_id, idempotency_key, request_hash, operation_type,
   resource_kind, resource_id, phase, state, progress, outcome,
   correlation_id, created_at, updated_at)
VALUES ($1,$2,$3,$4,'tunnel.route.reconcile','tunnel',$5,
        'persisting','pending',0,'unchanged',$6,now(),now())`, operationID, accountID, idempotencyKey, requestHash, resourceID, correlationID)
	return err
}

func trk35InsertOperationPGX(ctx context.Context, tx pgx.Tx, operationID, accountID, idempotencyKey string, requestHash []byte, resourceID, correlationID string) error {
	_, err := tx.Exec(ctx, `
INSERT INTO operations
  (id, account_id, idempotency_key, request_hash, operation_type,
   resource_kind, resource_id, phase, state, progress, outcome,
   correlation_id, created_at, updated_at)
VALUES ($1,$2,$3,$4,'tunnel.route.reconcile','tunnel',$5,
        'persisting','pending',0,'unchanged',$6,now(),now())`, operationID, accountID, idempotencyKey, requestHash, resourceID, correlationID)
	return err
}

func trk35InsertOperationIdempotent(ctx context.Context, tx *db.Tx, operationID, accountID, idempotencyKey string, requestHash []byte, resourceID, correlationID string) error {
	_, err := tx.Exec(ctx, `
INSERT INTO operations
  (id, account_id, idempotency_key, request_hash, operation_type,
   resource_kind, resource_id, phase, state, progress, outcome,
   correlation_id, created_at, updated_at)
VALUES ($1,$2,$3,$4,'tunnel.route.reconcile','tunnel',$5,
        'persisting','pending',0,'unchanged',$6,now(),now())
ON CONFLICT (account_id,idempotency_key) DO UPDATE
SET updated_at = operations.updated_at
WHERE operations.id = EXCLUDED.id
  AND operations.request_hash = EXCLUDED.request_hash`, operationID, accountID, idempotencyKey, requestHash, resourceID, correlationID)
	return err
}

func trk35InsertAudit(ctx context.Context, tx *db.Tx, auditID, accountID, eventType, resourceType, resourceID, idempotencyKey, correlationID string) error {
	_, err := tx.Exec(ctx, `
INSERT INTO audit_events
  (id, actor_user_id, actor_type, event_type, resource_type, resource_id,
   idempotency_key, metadata, created_at, account_id, actor_id, change_type,
   outcome, correlation_id)
VALUES ($1,NULL,'system',$2,$3,$4,$5,'{}'::jsonb,now(),$6,'trk35','changed','changed',$7)
ON CONFLICT (event_type,resource_type,resource_id,idempotency_key)
  WHERE idempotency_key IS NOT NULL DO NOTHING`, auditID, eventType, resourceType, resourceID, idempotencyKey, accountID, correlationID)
	return err
}

func trk35InsertAuditPGX(ctx context.Context, tx pgx.Tx, auditID, accountID, eventType, resourceType, resourceID, idempotencyKey, correlationID string) error {
	_, err := tx.Exec(ctx, `
INSERT INTO audit_events
  (id, actor_user_id, actor_type, event_type, resource_type, resource_id,
   idempotency_key, metadata, created_at, account_id, actor_id, change_type,
   outcome, correlation_id)
VALUES ($1,NULL,'system',$2,$3,$4,$5,'{}'::jsonb,now(),$6,'trk35','changed','changed',$7)
ON CONFLICT (event_type,resource_type,resource_id,idempotency_key)
  WHERE idempotency_key IS NOT NULL DO NOTHING`, auditID, eventType, resourceType, resourceID, idempotencyKey, accountID, correlationID)
	return err
}

func trk35AssertOperationAndAuditCount(t *testing.T, database *db.DB, operationID, auditID string, wantOperations, wantAudits int) {
	t.Helper()
	ctx := context.Background()
	var operations, audits int
	if err := database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.operations WHERE id=$1`, operationID).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if err := database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE id=$1`, auditID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if operations != wantOperations || audits != wantAudits {
		t.Fatalf("operation/audit rows = %d/%d, want %d/%d", operations, audits, wantOperations, wantAudits)
	}
}
