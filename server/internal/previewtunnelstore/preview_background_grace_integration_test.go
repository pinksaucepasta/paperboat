package previewtunnelstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// A host runtime that restarts (an update) must find its background preview still active: the
// owner grace for a host-owned preview with a user deadline is longer than for a foreground one.
func TestBackgroundPreviewKeepsItsURLThroughALongerOwnerGraceOnPostgres(t *testing.T) {
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
	accountID := "usr_bg_" + suffix
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1, $2, $3, 'active')`, accountID, "workos_bg_"+suffix, "bg-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	machine := "machine_bg_" + suffix
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,online,configured_capabilities) VALUES ($1,$2,$3,'linux','amd64','/workspace','online',true,ARRAY['preview_launch']::text[])`, machine, accountID, "env_"+machine); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.user_machines WHERE id=$1`, machine) }()
	now := time.Now().UTC().Truncate(time.Microsecond)
	create := func(name, kind string, userDeadline *time.Time) PreviewLeaseRecord {
		hash := sha256.Sum256([]byte(name + suffix))
		input := CreatePreviewLeaseV1Input{
			OperationID: "op_" + name + suffix, LeaseID: "prv_" + name + suffix, AuditEventID: "aud_" + name + suffix,
			AccountID: accountID, ActorID: accountID, ActorType: "user", OwnerMachineID: machine, OwnerSessionID: "session_" + name + suffix, OwnerSessionKind: kind,
			TargetScheme: "http", TargetAddress: "127.0.0.1:3000", AccessMode: "public", Endpoint: "https://" + name + suffix + ".preview.example.test", LeaseDeadline: now.Add(time.Hour),
			RequestHash: hash[:], IdempotencyKey: name + suffix, CorrelationID: "cor_" + name + suffix, RequestID: "req_" + name + suffix, SourceMachineID: machine, Now: now,
		}
		if userDeadline != nil {
			input.UserDeadline = sql.NullTime{Time: *userDeadline, Valid: true}
			input.LeaseDeadline = *userDeadline
		}
		created, err := store.CreatePreviewLeaseV1(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_leases SET owner_last_seen_at=$2 WHERE id=$1`, created.Lease.ID, now.Add(-2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		return created.Lease
	}
	deadline := now.Add(30 * time.Minute)
	foreground := create("foreground", "foreground", nil)
	background := create("background", "local_lease", &deadline)
	stale := create("stale", "local_lease", &deadline)
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_leases SET owner_last_seen_at=$2 WHERE id=$1`, stale.ID, now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	result, err := store.ReconcilePreviewLeasesV1(ctx, ReconcilePreviewLeasesV1Input{
		ActorID: accountID, ActorType: "system", CorrelationID: "cor_bg_reconcile_" + suffix, RequestID: "req_bg_reconcile_" + suffix,
		Now: now, OwnerGrace: 30 * time.Second, BackgroundOwnerGrace: 5 * time.Minute, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	lost := map[string]bool{}
	for _, record := range result.OwnerLost {
		lost[record.ID] = true
	}
	if !lost[foreground.ID] {
		t.Fatal("a foreground preview with a two-minute-old owner heartbeat stayed active")
	}
	if lost[background.ID] {
		t.Fatal("a background preview was lost 2 minutes after its host stopped, inside the longer background grace")
	}
	if !lost[stale.ID] {
		t.Fatal("a background preview whose host never came back was not released after the background grace")
	}
}
