package previewtunnelstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

// This test is intentionally opt-in. Run it against the isolated Hetzner
// PostgreSQL database with PAPERBOAT_TEST_DATABASE_DSN; it never starts local
// infrastructure.
func TestPreviewLeaseV1LifecycleOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run the preview lease PostgreSQL test")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	store, err := New(database)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	accountID := "usr_preview_" + suffix
	if _, err := database.SQL().ExecContext(ctx, `
INSERT INTO paperboat.users (id, workos_subject, primary_email, status)
VALUES ($1, $2, $3, 'active')`, accountID, "workos_preview_"+suffix, "preview-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	machineIDs := []string{"machine_" + suffix, "machine_expire_" + suffix, "machine_owner_" + suffix}
	for _, id := range machineIDs {
		if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,description,platform,architecture,workspace_root,state,online,configured_capabilities) VALUES($1,$2,$1,$1,'linux','amd64','/workspace','online',true,ARRAY['preview_launch']::text[])`, id, accountID); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		if _, err := database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.user_machines WHERE id=ANY($1::text[])`, machineIDs); err != nil {
			t.Error(err)
		}
	}()
	now := time.Now().UTC().Truncate(time.Microsecond)
	requestHash := sha256.Sum256([]byte("preview-create:" + suffix))
	createInput := CreatePreviewLeaseV1Input{
		OperationID: "op_preview_" + suffix, LeaseID: "prv_preview_" + suffix, AuditEventID: "aud_preview_" + suffix,
		AccountID: accountID, ActorID: accountID, ActorType: "user", OwnerMachineID: "machine_" + suffix, OwnerSessionID: "session_" + suffix, OwnerSessionKind: "foreground",
		TargetScheme: "http", TargetAddress: "127.0.0.1:3000", AccessMode: "public", Endpoint: "https://" + suffix + ".preview.example.test", LeaseDeadline: now.Add(2 * time.Minute),
		RequestHash: requestHash[:], IdempotencyKey: "preview-create:" + suffix, CorrelationID: "cor_preview_" + suffix,
		RequestID: "req_preview_" + suffix, SourceMachineID: "machine_" + suffix, Now: now,
	}
	created, err := store.CreatePreviewLeaseV1(ctx, createInput)
	if err != nil {
		t.Fatal(err)
	}
	if created.Lease.Generation != 1 || created.Operation.State != "running" {
		t.Fatalf("created lease = %#v operation = %#v", created.Lease, created.Operation)
	}
	if created.Lease.OwnerSessionKind != "foreground" {
		t.Fatalf("owner kind=%q", created.Lease.OwnerSessionKind)
	}
	changedKind := createInput
	changedKind.OwnerSessionKind = "local_lease"
	if _, err := store.CreatePreviewLeaseV1(ctx, changedKind); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("owner-kind replay conflict: %v", err)
	}
	for _, query := range []string{
		`UPDATE paperboat.preview_leases SET owner_session_kind='local_lease' WHERE id=$1`,
		`UPDATE paperboat.preview_leases SET owner_session_id='session_changed' WHERE id=$1`,
		`UPDATE paperboat.preview_leases SET owner_machine_id='machine_changed' WHERE id=$1`,
	} {
		if _, err := database.SQL().ExecContext(ctx, query, created.Lease.ID); err == nil {
			t.Fatal("immutable preview owner identity was changed")
		}
	}
	ready, err := store.MarkPreviewLeaseReadyV1(ctx, MarkPreviewLeaseReadyV1Input{
		AuditEventID: "aud_ready_" + suffix, AccountID: accountID, ActorID: accountID, ActorType: "user",
		PreviewID: created.Lease.ID, ExpectedGeneration: 1, AllocationState: "ready", EdgeState: "ready", OriginState: "ready",
		CorrelationID: "cor_ready_" + suffix, RequestID: "req_ready_" + suffix, SourceMachineID: "machine_" + suffix, Now: now.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready.Generation != 2 || !ready.ReadyAt.Valid {
		t.Fatalf("ready lease = %#v", ready)
	}
	operation, err := store.GetOperation(ctx, accountID, created.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if operation.State != "succeeded" || operation.Phase != "ready" || operation.Progress != 100 {
		t.Fatalf("create operation was not completed: %#v", operation)
	}

	renewed, err := store.RenewPreviewLeaseV1(ctx, RenewPreviewLeaseV1Input{
		OperationID: "op_renew_" + suffix, AuditEventID: "aud_renew_" + suffix, AccountID: accountID, ActorID: accountID, ActorType: "user",
		PreviewID: created.Lease.ID, OwnerMachineID: "machine_" + suffix, OwnerSessionID: "session_" + suffix, ExpectedGeneration: 2, LeaseDeadline: now.Add(3 * time.Minute), RequestHash: sha256Bytes("preview-renew:" + suffix),
		IdempotencyKey: "preview-renew:" + suffix, CorrelationID: "cor_renew_" + suffix, RequestID: "req_renew_" + suffix,
		SourceMachineID: "machine_" + suffix, Now: now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Lease.Generation != 3 || !renewed.Lease.LastRenewedAt.After(ready.LastRenewedAt) {
		t.Fatalf("renewed lease = %#v", renewed.Lease)
	}

	stopped, err := store.StopPreviewLeaseV1(ctx, StopPreviewLeaseV1Input{
		OperationID: "op_stop_" + suffix, AuditEventID: "aud_stop_" + suffix, AccountID: accountID, ActorID: accountID, ActorType: "user",
		PreviewID: created.Lease.ID, OwnerMachineID: "machine_" + suffix, OwnerSessionID: "session_" + suffix, ExpectedGeneration: 3, RequestHash: sha256Bytes("preview-stop:" + suffix), IdempotencyKey: "preview-stop:" + suffix,
		CorrelationID: "cor_stop_" + suffix, RequestID: "req_stop_" + suffix, SourceMachineID: "machine_" + suffix, Now: now.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Lease.Generation != 4 || stopped.Lease.TerminalState != "stopped" || !stopped.Lease.StoppedAt.Valid {
		t.Fatalf("stopped lease = %#v", stopped.Lease)
	}
	keyReplay, err := store.StopPreviewLeaseV1(ctx, StopPreviewLeaseV1Input{
		OperationID: "op_stop_key_replay_" + suffix, AuditEventID: "aud_stop_key_replay_" + suffix, AccountID: accountID, ActorID: accountID, ActorType: "user",
		PreviewID: created.Lease.ID, OwnerMachineID: "machine_" + suffix, OwnerSessionID: "session_" + suffix, ExpectedGeneration: 999, RequestHash: sha256Bytes("preview-stop:" + suffix), IdempotencyKey: "preview-stop:" + suffix,
		CorrelationID: "cor_stop_key_replay_" + suffix, RequestID: "req_stop_key_replay_" + suffix, SourceMachineID: "machine_" + suffix, Now: now.Add(4 * time.Second),
	})
	if err != nil || !keyReplay.Replayed {
		t.Fatalf("same-key stop replay = %#v, %v", keyReplay, err)
	}
	if _, err := store.StopPreviewLeaseV1(ctx, StopPreviewLeaseV1Input{
		OperationID: "op_stop_conflict_" + suffix, AuditEventID: "aud_stop_conflict_" + suffix, AccountID: accountID, ActorID: accountID, ActorType: "user",
		PreviewID: created.Lease.ID, OwnerMachineID: "machine_" + suffix, OwnerSessionID: "session_" + suffix, ExpectedGeneration: 3, RequestHash: sha256Bytes("different-stop:" + suffix), IdempotencyKey: "preview-stop:" + suffix,
		CorrelationID: "cor_stop_conflict_" + suffix, RequestID: "req_stop_conflict_" + suffix, SourceMachineID: "machine_" + suffix, Now: now.Add(5 * time.Second),
	}); err != ErrIdempotencyConflict {
		t.Fatalf("same-key stop hash conflict = %v", err)
	}
	replayed, err := store.StopPreviewLeaseV1(ctx, StopPreviewLeaseV1Input{
		OperationID: "op_stop_replay_" + suffix, AuditEventID: "aud_stop_replay_" + suffix, AccountID: accountID, ActorID: accountID, ActorType: "user",
		PreviewID: created.Lease.ID, OwnerMachineID: "machine_" + suffix, OwnerSessionID: "session_" + suffix, ExpectedGeneration: 999, RequestHash: sha256Bytes("preview-stop-replay:" + suffix), IdempotencyKey: "preview-stop-replay:" + suffix,
		CorrelationID: "cor_stop_replay_" + suffix, RequestID: "req_stop_replay_" + suffix, SourceMachineID: "machine_" + suffix, Now: now.Add(4 * time.Second),
	})
	if err != nil || !replayed.Replayed {
		t.Fatalf("terminal stop idempotency = %#v, %v", replayed, err)
	}

	// Runtime cleanup can arrive at the deadline before the expiry worker.
	// That ordering must retain expiry as the terminal outcome.
	dueInput := createInput
	dueInput.OperationID, dueInput.LeaseID = "op_stop_due_"+suffix, "prv_stop_due_"+suffix
	dueInput.AuditEventID = "aud_stop_due_create_" + suffix
	dueInput.Endpoint = "https://stop-due-" + suffix + ".preview.example.test"
	dueInput.IdempotencyKey = "create-stop-due:" + suffix
	dueInput.RequestHash = sha256Bytes(dueInput.IdempotencyKey)
	dueInput.LeaseDeadline = now.Add(5 * time.Second)
	due, err := store.CreatePreviewLeaseV1(ctx, dueInput)
	if err != nil {
		t.Fatal(err)
	}
	dueStopped, err := store.StopPreviewLeaseV1(ctx, StopPreviewLeaseV1Input{
		OperationID: "op_stop_due_cleanup_" + suffix, AuditEventID: "aud_stop_due_cleanup_" + suffix,
		AccountID: accountID, ActorID: accountID, ActorType: "user", PreviewID: due.Lease.ID,
		OwnerMachineID: due.Lease.OwnerMachineID, OwnerSessionID: due.Lease.OwnerSessionID,
		ExpectedGeneration: 1, RequestHash: sha256Bytes("stop-due:" + suffix), IdempotencyKey: "stop-due:" + suffix,
		CorrelationID: "cor_stop_due_" + suffix, RequestID: "req_stop_due_" + suffix, Now: dueInput.LeaseDeadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if dueStopped.Lease.TerminalState != "expired" || dueStopped.Lease.Generation != 2 {
		t.Fatalf("cleanup at deadline lost expiry: state=%s generation=%d", dueStopped.Lease.TerminalState, dueStopped.Lease.Generation)
	}

	expireHash := sha256Bytes("preview-expire:" + suffix)
	expired, err := store.CreatePreviewLeaseV1(ctx, CreatePreviewLeaseV1Input{
		OperationID: "op_expire_" + suffix, LeaseID: "prv_expire_" + suffix, AuditEventID: "aud_expire_" + suffix,
		AccountID: accountID, ActorID: accountID, ActorType: "user", OwnerMachineID: "machine_expire_" + suffix, OwnerSessionID: "session_expire_" + suffix, OwnerSessionKind: "local_lease",
		TargetScheme: "http", TargetAddress: "localhost:3000", AccessMode: "private", Endpoint: "https://expire-" + suffix + ".preview.example.test", LeaseDeadline: now.Add(5 * time.Second), RequestHash: expireHash,
		IdempotencyKey: "preview-expire:" + suffix, CorrelationID: "cor_expire_" + suffix, RequestID: "req_expire_" + suffix, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if expired.Lease.OwnerSessionKind != "local_lease" {
		t.Fatal("local lease kind lost in storage")
	}
	ownerHash := sha256Bytes("preview-owner:" + suffix)
	ownerLost, err := store.CreatePreviewLeaseV1(ctx, CreatePreviewLeaseV1Input{
		OperationID: "op_owner_" + suffix, LeaseID: "prv_owner_" + suffix, AuditEventID: "aud_owner_" + suffix,
		AccountID: accountID, ActorID: accountID, ActorType: "user", OwnerMachineID: "machine_owner_" + suffix, OwnerSessionID: "session_owner_" + suffix, OwnerSessionKind: "foreground",
		TargetScheme: "http", TargetAddress: "127.0.0.1:4000", AccessMode: "public", Endpoint: "https://owner-" + suffix + ".preview.example.test", LeaseDeadline: now.Add(time.Hour), RequestHash: ownerHash,
		IdempotencyKey: "preview-owner:" + suffix, CorrelationID: "cor_owner_" + suffix, RequestID: "req_owner_" + suffix, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_leases SET owner_last_seen_at=$2 WHERE id=$1`, ownerLost.Lease.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A still-owned lease remains active while allocation or reconnect is pending.
	for _, state := range []string{"allocating", "connecting", "ready"} {
		live := createInput
		live.OperationID, live.LeaseID, live.AuditEventID = "op_live_"+state+suffix, "prv_live_"+state+suffix, "aud_live_"+state+suffix
		live.Endpoint = "https://live-" + state + suffix + ".preview.example.test"
		live.IdempotencyKey, live.RequestHash = "live-"+state+suffix, sha256Bytes("live-"+state+suffix)
		createdLive, err := store.CreatePreviewLeaseV1(ctx, live)
		if err != nil {
			t.Fatal(err)
		}
		if state != "allocating" {
			edge, origin := "pending", "unknown"
			if state == "ready" {
				edge, origin = "ready", "ready"
			}
			if _, err := store.MarkPreviewLeaseReadyV1(ctx, MarkPreviewLeaseReadyV1Input{
				AuditEventID: "aud_live_ready_" + state + suffix, AccountID: accountID, ActorID: accountID, ActorType: "user",
				PreviewID: createdLive.Lease.ID, ExpectedGeneration: 1, AllocationState: "ready", EdgeState: edge, OriginState: origin,
				CorrelationID: "cor_live_" + state + suffix, RequestID: "req_live_" + state + suffix, Now: now.Add(time.Second),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	reconciled, err := store.ReconcilePreviewLeasesV1(ctx, ReconcilePreviewLeasesV1Input{
		ActorID: accountID, ActorType: "system", CorrelationID: "cor_reconcile_" + suffix, RequestID: "req_reconcile_" + suffix,
		Now: now.Add(10 * time.Second), OwnerGrace: 30 * time.Second, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Reconciliation is global; other integration fixtures may also be due.
	// Keep exact assertions for every lease owned by this test's account.
	owned := func(rows []PreviewLeaseRecord) []PreviewLeaseRecord {
		var result []PreviewLeaseRecord
		for _, row := range rows {
			if row.AccountID == accountID {
				result = append(result, row)
			}
		}
		return result
	}
	reconciled.Expired, reconciled.OwnerLost = owned(reconciled.Expired), owned(reconciled.OwnerLost)
	if len(reconciled.Expired) != 1 || reconciled.Expired[0].ID != expired.Lease.ID || len(reconciled.OwnerLost) != 1 || reconciled.OwnerLost[0].ID != ownerLost.Lease.ID {
		t.Fatalf("reconciliation did not return exact fixture leases: expired=%d owner_lost=%d", len(reconciled.Expired), len(reconciled.OwnerLost))
	}
	if reconciled.Expired[0].Generation != 2 || reconciled.OwnerLost[0].Generation != 2 {
		t.Fatalf("reconciliation did not advance generation: expired=%d owner_lost=%d", reconciled.Expired[0].Generation, reconciled.OwnerLost[0].Generation)
	}
	activeContext := workspace.WithInventoryFilters(ctx, workspace.InventoryFilters{State: "active"})
	active, err := store.ListPreviewLeasesV1(activeContext, ListPreviewLeasesV1Input{AccountID: accountID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 3 {
		t.Fatalf("active inventory count=%d want 3 live leases", len(active))
	}
	for _, lease := range active {
		if lease.TerminalState != "active" {
			t.Fatal("active inventory contains terminal lease")
		}
	}
	historyContext := workspace.WithInventoryFilters(ctx, workspace.InventoryFilters{State: "owner_disconnected"})
	history, err := store.ListPreviewLeasesV1(historyContext, ListPreviewLeasesV1Input{AccountID: accountID, Limit: 100})
	if err != nil || len(history) != 1 || history[0].ID != ownerLost.Lease.ID || !history[0].StoppedAt.Valid {
		t.Fatalf("owner-disconnected history count=%d err=%v", len(history), err)
	}
	if _, err := store.RenewPreviewLeaseV1(ctx, RenewPreviewLeaseV1Input{
		OperationID: "op_lost_renew_" + suffix, AuditEventID: "aud_lost_renew_" + suffix, AccountID: accountID, ActorID: accountID, ActorType: "user",
		PreviewID: ownerLost.Lease.ID, OwnerMachineID: ownerLost.Lease.OwnerMachineID, OwnerSessionID: ownerLost.Lease.OwnerSessionID,
		ExpectedGeneration: 2, LeaseDeadline: now.Add(2 * time.Hour), RequestHash: sha256Bytes("lost-renew" + suffix),
		IdempotencyKey: "lost-renew" + suffix, CorrelationID: "cor_lost_renew_" + suffix, RequestID: "req_lost_renew_" + suffix, Now: now.Add(11 * time.Second),
	}); !errors.Is(err, ErrPreviewLeaseTerminal) {
		t.Fatalf("owner-lost renewal: %v", err)
	}

}

func sha256Bytes(value string) []byte {
	hash := sha256.Sum256([]byte(value))
	return hash[:]
}

func TestSharedPreviewMachineAuthorityOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	tx, err := database.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, member, team, machine := "sp_owner_"+suffix, "sp_member_"+suffix, "sp_team_"+suffix, "sp_machine_"+suffix
	for _, account := range []string{owner, member} {
		if _, err = tx.Exec(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, account); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner); err != nil {
		t.Fatal(err)
	}
	// Shared preview authority requires an active paid team as well as an exact grant.
	if _, err = tx.Exec(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',2,now()-interval '1 hour',now()+interval '1 hour')`, "shared-preview-billing-"+suffix, team); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'member',true)`, team, owner, member); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,description,platform,architecture,workspace_root,state,online,configured_capabilities) VALUES($1,$2,$1,$1,'linux','amd64','/workspace','online',true,ARRAY['preview_launch']::text[])`, machine, owner); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'machine',$2,$3)`, team, machine, owner); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO paperboat.team_machine_grants(team_id,machine_id,audience,account_id,capabilities) VALUES($1,$2,'selected_member',$3,ARRAY['preview_manage']::text[])`, team, machine, member); err != nil {
		t.Fatal(err)
	}
	check := func(actor, mode string, wantShared bool, wantErr error) {
		t.Helper()
		shared, e := verifyPreviewMachineAuthority(tx.QueryRow(ctx, previewMachineAuthoritySQL, actor, machine), mode)
		if shared != wantShared || !errors.Is(e, wantErr) {
			t.Fatalf("%s %s shared=%v err=%v", actor, mode, shared, e)
		}
	}
	check(owner, "public", false, nil)
	check(member, "private", true, nil)
	check(member, "team", true, nil)
	check(member, "public", false, ErrPreviewPublicationDenied)
	if _, err = tx.Exec(ctx, `UPDATE paperboat.user_machines SET owner_team_id=$1 WHERE id=$2`, team, machine); err != nil {
		t.Fatal(err)
	}
	check(owner, "private", false, ErrOwnerNotFound)
	check(member, "private", true, nil)
	if _, err = tx.Exec(ctx, `UPDATE paperboat.user_machines SET configured_capabilities='{}' WHERE id=$1`, machine); err != nil {
		t.Fatal(err)
	}
	check(member, "private", false, ErrOwnerNotFound)
	if _, err = tx.Exec(ctx, `UPDATE paperboat.user_machines SET configured_capabilities=ARRAY['preview_launch']::text[] WHERE id=$1`, machine); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE paperboat.team_members SET active=false WHERE team_id=$1 AND account_id=$2`, team, member); err != nil {
		t.Fatal(err)
	}
	check(member, "private", false, ErrOwnerNotFound)
}
