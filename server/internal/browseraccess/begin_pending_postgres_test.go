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
