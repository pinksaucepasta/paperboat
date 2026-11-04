package httpapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

// TestE2EServe is the server half of the CI end-to-end job (tools/e2e/run.sh): the real services behind
// the real router on a TLS listener, one account with two one-shot enrollment tokens, and only the
// identity provider replaced. It runs until the stop file appears in PAPERBOAT_E2E_DIR.
func TestE2EServe(t *testing.T) {
	directory := os.Getenv("PAPERBOAT_E2E_DIR")
	if directory == "" {
		t.Skip("CI end-to-end server only")
	}
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	address, base := os.Getenv("PAPERBOAT_E2E_LISTEN"), os.Getenv("PAPERBOAT_E2E_URL")
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	router, machines := task39Stack(t, database, base)
	email := "e2e-" + time.Now().UTC().Format("20060102150405") + "@example.test"
	loginCookies(t, router, "e2e_"+email+":"+email+":e2e")
	account := userIDByEmail(t, database, email)
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'connected-test','active',2,1073741824,now()-interval '1 hour',now()+interval '1 day')`, "ume_e2e_"+account, account, "sub_e2e_"+account); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{}
	for _, name := range []string{"e2e-a", "e2e-b"} {
		started, err := machines.StartEnrollmentWithOptions(ctx, account, "e2e_"+name, usermachines.EnrollmentOptions{Shell: "posix"})
		if err != nil {
			t.Fatal(err)
		}
		tokens[name] = started.BootstrapToken
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: router, ReadHeaderTimeout: 5 * time.Second}
	finished := make(chan error, 1)
	go func() {
		finished <- server.ServeTLS(listener, os.Getenv("PAPERBOAT_E2E_TLS_CERT"), os.Getenv("PAPERBOAT_E2E_TLS_KEY"))
	}()
	defer server.Close()
	payload, _ := json.Marshal(tokens)
	if err := os.WriteFile(filepath.Join(directory, "tokens.json"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	for tick := time.NewTicker(time.Second); ; <-tick.C {
		select {
		case err := <-finished:
			t.Fatalf("e2e listener stopped: %v", err)
		default:
		}
		if _, err := os.Stat(filepath.Join(directory, "stop")); err == nil {
			return
		}
	}
}
