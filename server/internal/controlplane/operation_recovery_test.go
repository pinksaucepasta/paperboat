package controlplane

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func TestOperationRecoveryIsAuditedAndIdempotent(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	actorID := "recovery_user_" + suffix
	operationID := "recovery_op_" + suffix
	seedRecoveryUser(t, store, actorID, suffix)
	setOperatorRole(t, store, actorID, "operations")
	seedControlOperation(t, store, operationID, "recovery_source_"+suffix)
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_operations SET state='dead_letter', attempts=4, last_error='terminal failure' WHERE id=$1`, operationID); err != nil {
		t.Fatal(err)
	}

	service := NewOperationRecoveryService(store, nil)
	key := "recover_key_" + suffix
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if err := service.Recover(ctx, actorID, key, operationID, now); err != nil {
		t.Fatal(err)
	}
	if err := service.Recover(ctx, actorID, key, operationID, now.Add(time.Minute)); err != nil {
		t.Fatalf("exact replay: %v", err)
	}

	var state string
	var attempts int
	if err := store.SQL().QueryRowContext(ctx, `SELECT state, attempts FROM paperboat.control_operations WHERE id=$1`, operationID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || attempts != 0 {
		t.Fatalf("state/attempts = %s/%d, want pending/0", state, attempts)
	}
	var audits int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE event_type='control.operation_recovered' AND resource_id=$1 AND idempotency_key=$2`, operationID, key).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("audit count = %d, want 1", audits)
	}

	otherID := "recovery_other_" + suffix
	seedControlOperation(t, store, otherID, "recovery_other_key_"+suffix)
	if err := service.Recover(ctx, actorID, key, otherID, now); !errors.Is(err, ErrRecoveryKeyConflict) {
		t.Fatalf("conflicting replay error = %v, want ErrRecoveryKeyConflict", err)
	}
	setOperatorRole(t, store, actorID, "user")
	if err := service.Recover(ctx, actorID, key, operationID, now); !errors.Is(err, ErrOperationRecoveryForbidden) {
		t.Fatalf("demoted replay error = %v, want ErrOperationRecoveryForbidden", err)
	}
}

func TestAdminUsageKeyMutationBindsReplayAndRechecksRole(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	actorID, nodeID := "usage_admin_"+suffix, "usage_node_"+suffix
	seedRecoveryUser(t, store, actorID, "usage_"+suffix)
	setOperatorRole(t, store, actorID, "admin")
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch) VALUES($1,'default','1.0',$2)`, nodeID, "usage_epoch_"+suffix); err != nil {
		t.Fatal(err)
	}
	service := NewEdgeService(store, "test")
	keyID, operationKey := "usage_key_"+suffix, "usage_provision_"+suffix
	publicKey := ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))
	notBefore := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	if err := service.ProvisionUsageKey(ctx, actorID, operationKey, keyID, nodeID, publicKey, notBefore, notBefore.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := service.ProvisionUsageKey(ctx, actorID, operationKey, keyID, nodeID, publicKey, notBefore, notBefore.Add(time.Hour)); err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	changed := append(ed25519.PublicKey(nil), publicKey...)
	changed[0] = 1
	if err := service.ProvisionUsageKey(ctx, actorID, operationKey, keyID, nodeID, changed, notBefore, notBefore.Add(time.Hour)); !errors.Is(err, ErrUsageOperationConflict) {
		t.Fatalf("changed replay error = %v, want ErrUsageOperationConflict", err)
	}
	setOperatorRole(t, store, actorID, "support")
	if err := service.ProvisionUsageKey(ctx, actorID, operationKey, keyID, nodeID, publicKey, notBefore, notBefore.Add(time.Hour)); !errors.Is(err, ErrAdminMutationForbidden) {
		t.Fatalf("support replay error = %v, want ErrAdminMutationForbidden", err)
	}
	var audits int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE event_type='edge.usage_key_provisioned' AND resource_id=$1 AND idempotency_key=$2`, keyID, operationKey).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit count=%d err=%v", audits, err)
	}
}

func TestOperationRecoveryRollsBackReservationForNonDeadLetter(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	operationID := "non_dead_op_" + suffix
	seedControlOperation(t, store, operationID, "non_dead_source_"+suffix)
	key := "non_dead_recovery_" + suffix

	actorID := "non_dead_actor_" + suffix
	seedRecoveryUser(t, store, actorID, "non_dead_"+suffix)
	setOperatorRole(t, store, actorID, "admin")
	service := NewOperationRecoveryService(store, nil)
	if err := service.Recover(ctx, actorID, key, operationID, time.Now()); !errors.Is(err, ErrOperationNotDeadLettered) {
		t.Fatalf("recovery error = %v, want ErrOperationNotDeadLettered", err)
	}
	var count int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.control_operation_recoveries WHERE operation_key=$1`, key).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("recovery reservation count = %d, want 0", count)
	}
}

func seedRecoveryUser(t *testing.T, store *db.DB, actorID, suffix string) {
	t.Helper()
	if _, err := store.SQL().ExecContext(context.Background(), `
		INSERT INTO paperboat.users (id, workos_subject, primary_email, status)
		VALUES ($1, $2, $3, 'active')`, actorID, "workos_"+suffix, suffix+"@recovery.example.test"); err != nil {
		t.Fatal(err)
	}
}

// Operator permissions live in operator_accounts, separate from customer users;
// a customer role grants nothing. "user" means no operator account.
func setOperatorRole(t *testing.T, store *db.DB, actorID, role string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.SQL().ExecContext(ctx, `DELETE FROM paperboat.operator_accounts WHERE id=$1`, actorID); err != nil {
		t.Fatal(err)
	}
	if role == "user" {
		return
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash,role,status) VALUES($1,$1||'@operator.example.test','unused test login',$2,'active')`, actorID, role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.operator_accounts WHERE id=$1`, actorID)
	})
}
