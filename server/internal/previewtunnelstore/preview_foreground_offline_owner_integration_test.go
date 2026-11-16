package previewtunnelstore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A foreground preview lives as long as its command runs, however long its machine is offline or asleep:
// the owner grace only applies once the owner is online (and settled) again, and a removed owner ends it.
func TestForegroundPreviewSurvivesAnOfflineOwnerOnPostgres(t *testing.T) {
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
	accountID := "usr_fgoff_" + suffix
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1, $2, $3, 'active')`, accountID, "workos_fgoff_"+suffix, "fgoff-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	type fixture struct {
		machine string
		lease   PreviewLeaseRecord
	}
	// owner is applied to the machine after the lease exists (create needs an online owner).
	create := func(name, kind string, userDeadline *time.Time, owner string) fixture {
		machine := "machine_" + name + "_" + suffix
		if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,online,configured_capabilities) VALUES ($1,$2,$3,'linux','amd64','/workspace','online',true,ARRAY['preview_launch']::text[])`, machine, accountID, "env_"+machine); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		})
		hash := sha256.Sum256([]byte(name + suffix))
		input := CreatePreviewLeaseV1Input{
			OperationID: "op_" + name + suffix, LeaseID: "prv_" + name + suffix, AuditEventID: "aud_" + name + suffix,
			AccountID: accountID, ActorID: accountID, ActorType: "user", OwnerMachineID: machine, OwnerSessionID: "session_" + name + suffix, OwnerSessionKind: kind,
			TargetScheme: "http", TargetAddress: "127.0.0.1:3000", AccessMode: "public", Endpoint: "https://" + name + suffix + ".preview.example.test", LeaseDeadline: now.Add(time.Hour),
			RequestHash: hash[:], IdempotencyKey: name + suffix, CorrelationID: "cor_" + name + suffix, RequestID: "req_" + name + suffix, SourceMachineID: machine, Now: now,
		}
		if userDeadline != nil {
			input.UserDeadline.Time, input.UserDeadline.Valid = *userDeadline, true
			input.LeaseDeadline = *userDeadline
		}
		created, err := store.CreatePreviewLeaseV1(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		// The last renew was 20 minutes ago, past every grace.
		if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_leases SET owner_last_seen_at=$2 WHERE id=$1`, created.Lease.ID, now.Add(-20*time.Minute)); err != nil {
			t.Fatal(err)
		}
		var ownerSQL string
		switch owner {
		case "offline":
			ownerSQL = `UPDATE paperboat.user_machines SET state='offline', online=false, updated_at=$2 WHERE id=$1`
		case "online":
			ownerSQL = `UPDATE paperboat.user_machines SET last_seen_at=$2, updated_at=$2 WHERE id=$1`
		case "woke":
			ownerSQL = `UPDATE paperboat.user_machines SET last_seen_at=$2, updated_at=$2 WHERE id=$1`
		case "revoked":
			ownerSQL = `UPDATE paperboat.user_machines SET state='revoked', updated_at=$2 WHERE id=$1`
		}
		at := now.Add(-time.Hour)
		if owner == "woke" {
			at = now.Add(-2 * time.Second)
		}
		if _, err := database.SQL().ExecContext(ctx, ownerSQL, machine, at); err != nil {
			t.Fatal(err)
		}
		return fixture{machine: machine, lease: created.Lease}
	}
	lapse := func(f fixture) {
		if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_leases SET created_at=$2::timestamptz-interval '2 hours', lease_deadline=$2 WHERE id=$1`, f.lease.ID, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := now.Add(30 * time.Minute)
	asleep := create("asleep", "foreground", nil, "offline")
	asleepLapsed := create("asleeplapsed", "local_lease", nil, "offline")
	lapse(asleepLapsed)
	dead := create("dead", "foreground", nil, "online")
	deadLapsed := create("deadlapsed", "foreground", nil, "online")
	lapse(deadLapsed)
	woke := create("woke", "foreground", nil, "woke")
	wokeLapsed := create("wokelapsed", "foreground", nil, "woke")
	lapse(wokeLapsed)
	removed := create("removed", "foreground", nil, "revoked")
	background := create("background", "local_lease", &deadline, "offline")
	result, err := store.ReconcilePreviewLeasesV1(ctx, ReconcilePreviewLeasesV1Input{
		ActorID: accountID, ActorType: "system", CorrelationID: "cor_fgoff_reconcile_" + suffix, RequestID: "req_fgoff_reconcile_" + suffix,
		Now: now, OwnerGrace: 30 * time.Second, BackgroundOwnerGrace: 5 * time.Minute, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	ended := map[string]string{}
	for _, record := range result.OwnerLost {
		ended[record.ID] = "owner_lost"
	}
	for _, record := range result.Expired {
		ended[record.ID] = "expired"
	}
	for _, c := range []struct {
		name string
		f    fixture
		want string // "" = stays active
	}{
		{"foreground, owner asleep, renewed 20 min ago", asleep, ""},
		{"foreground, owner asleep, lease deadline lapsed", asleepLapsed, ""},
		{"foreground, owner online and settled, no renew", dead, "owner_lost"},
		{"foreground, owner online and settled, lease deadline lapsed", deadLapsed, "expired"},
		{"foreground, owner woke 2 s ago, not renewed yet", woke, ""},
		{"foreground, owner woke 2 s ago, lease deadline lapsed", wokeLapsed, ""},
		{"foreground, owner revoked", removed, "owner_lost"},
		{"background, owner offline past the background grace", background, "owner_lost"},
	} {
		if got := ended[c.f.lease.ID]; got != c.want {
			t.Errorf("%s: ended as %q, want %q", c.name, got, c.want)
		}
	}
}
