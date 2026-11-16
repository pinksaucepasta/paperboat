package browseraccess

import (
	"context"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A host under the machine-service domain with no route yet must start a sign-in (it was a 503 check violation).
func TestBeginPendingMachineServiceHostPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	store, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx := context.Background()
	if e := db.Migrate(ctx, store); e != nil {
		t.Fatal(e)
	}
	svc, e := NewService(store, "https://console.example.test")
	if e != nil {
		t.Fatal(e)
	}
	svc.MachineServiceDomain = "preview.example.test"
	out, e := svc.Begin(ctx, BeginRequest{Host: "no-route-yet.preview.example.test", ReturnPath: "/"})
	if e != nil || out.TransactionID == "" {
		t.Fatalf("begin: %+v %v", out, e)
	}
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.browser_access_transactions WHERE transaction_id=$1`, out.TransactionID)
}

// A sign-in burst at one resource refuses only that resource (it was one server-wide cap: any burst refused everyone).
func TestBeginCapacityIsPerResourcePostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	store, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx := context.Background()
	if e := db.Migrate(ctx, store); e != nil {
		t.Fatal(e)
	}
	svc, e := NewService(store, "https://console.example.test")
	if e != nil {
		t.Fatal(e)
	}
	svc.MachineServiceDomain = "preview.example.test"
	busy, quiet := "busy.preview.example.test", "quiet.preview.example.test"
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.browser_access_transactions WHERE hostname IN ($1,$2)`, busy, quiet)
	if _, e := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.browser_access_transactions(transaction_id,state_hash,hostname,return_path,resource_kind,resource_id,route_id,created_at,expires_at)
SELECT 'transaction_capfill_'||g, sha256(('capfill'||g)::bytea), $1, '/', 'machine_service_pending', $1, $1, now(), now()+interval '10 minutes'
FROM generate_series(1,$2::int) g`, busy, pendingBeginsPerResource); e != nil {
		t.Fatal(e)
	}
	if _, e := svc.Begin(ctx, BeginRequest{Host: busy, ReturnPath: "/"}); e != ErrCapacity {
		t.Fatalf("busy resource begin: %v, want ErrCapacity", e)
	}
	if out, e := svc.Begin(ctx, BeginRequest{Host: quiet, ReturnPath: "/"}); e != nil || out.TransactionID == "" {
		t.Fatalf("other resource begin: %+v %v", out, e)
	}
}
