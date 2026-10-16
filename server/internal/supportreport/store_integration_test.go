package supportreport

import (
	"context"
	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSupportPostgresIsolationRetentionAndAudit(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated remote PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	ref := NewReference()
	ownerA := "user_" + uuid.NewString()
	ownerB := "user_" + uuid.NewString()
	operator := "opr_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	for _, user := range []string{ownerA, ownerB} {
		if _, err = database.Pool().Exec(ctx, `INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, user, user+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		database.Pool().Exec(ctx, `DELETE FROM support_reports WHERE support_reference=$1`, ref)
		database.Pool().Exec(ctx, `DELETE FROM audit_events WHERE resource_id=$1`, ref)
		database.Pool().Exec(ctx, `DELETE FROM users WHERE id IN ($1,$2)`, ownerA, ownerB)
		database.Pool().Exec(ctx, `DELETE FROM operator_accounts WHERE id=$1`, operator)
	}()
	store, _ := NewSQLStore(database)
	service, _ := New(store, Config{})
	first := Record{ID: NewReference(), Reference: ref, OwnerID: ownerA, RequestID: "req_a", Component: "paperboat-server", Release: "test", Stage: "authorize", Code: "internal_error", At: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour), Reporting: "disabled", Operation: "http_other", Cause: "internal", ErrorType: "none", Outcome: "failed", Severity: "error"}
	if err = store.Save(ctx, first); err != nil {
		t.Fatal(err)
	}
	writes := make(chan error, 40)
	for i := 0; i < 40; i++ {
		r := first
		r.ID = NewReference()
		r.At = r.At.Add(time.Duration(i+1) * time.Millisecond)
		r.Stage = "retry"
		go func() { writes <- store.Save(ctx, r) }()
	}
	for i := 0; i < 40; i++ {
		if err := <-writes; err != nil {
			t.Fatal(err)
		}
	}
	r := first
	r.ID = NewReference()
	r.OwnerID = ownerB
	r.Code = "other_owner"
	if err = store.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	a, err := service.Lookup(ctx, ownerA, ownerA, ref, false)
	if err != nil || len(a) != 32 || a[0].Stage != "authorize" || len(a[0].ErrorChain) != 0 {
		t.Fatalf("first failure/cap: %d %v", len(a), err)
	}
	b, err := service.Lookup(ctx, ownerB, ownerB, ref, false)
	if err != nil || len(b) != 1 || b[0].Code != "other_owner" {
		t.Fatal("owner collision was not isolated", err)
	}
	if _, err = service.Lookup(ctx, ownerA, "", ref, true); err == nil {
		t.Fatal("ordinary user obtained operator evidence")
	}
	if _, err = database.Pool().Exec(ctx, `INSERT INTO operator_accounts(id,email,password_hash,role,status) VALUES($1,$2,'fixture-not-used-for-login','support','active')`, operator, operator+"@example.test"); err != nil {
		t.Fatal(err)
	}
	all, err := service.Lookup(ctx, operator, "", ref, true)
	if err != nil || len(all) != 33 {
		t.Fatal("single-reference operator lookup failed", err, len(all))
	}
	owners := map[string]bool{}
	for _, record := range all {
		owners[record.OwnerID] = true
	}
	if len(owners) != 2 {
		t.Fatal("reference collision merged owners")
	}
	if _, err = store.Lookup(ctx, "missing_audit_actor", ownerA, ref, true); err == nil {
		t.Fatal("evidence disclosed without durable audit")
	}
	var auditCount int
	if err = database.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_id=$1 AND event_type='support.self_lookup'`, ref).Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatal("lookup audit missing", err, auditCount)
	}
	if _, err = database.Pool().Exec(ctx, `UPDATE support_reports SET expires_at=now()-interval '1 second' WHERE support_reference=$1`, ref); err != nil {
		t.Fatal(err)
	}
	a, err = service.Lookup(ctx, ownerA, ownerA, ref, false)
	if err != nil || len(a) != 0 {
		t.Fatal("expired evidence disclosed")
	}
	if err = store.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	database.Pool().QueryRow(ctx, `SELECT count(*) FROM support_reports WHERE support_reference=$1`, ref).Scan(&count)
	if count != 0 {
		t.Fatal("retained expired data")
	}
}
