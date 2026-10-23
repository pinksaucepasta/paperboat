package environment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A revoked (disconnected) machine is not an ENV vault host any more: no layer context, and no rotated delivery
// is addressed to its host key. Its host row stays; the machine state decides.
func TestDisconnectedMachineIsNoVaultHostOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set isolated PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, machine, environment := "vh_owner_"+suffix, "vh_machine_"+suffix, "vh_env_"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM user_machines WHERE id=$1`, machine)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM users WHERE id=$1`, owner)
	})
	exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, owner, owner+"@example.test")
	exec(`INSERT INTO control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, owner)
	exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation,configured_capabilities) VALUES($1,$2,$3,'vh','linux','amd64','/w','online','occupied',true,1,ARRAY['environment_injection'])`, machine, owner, environment)
	authorize := func() error {
		return store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			_, e := authorizeVaultHostTx(ctx, tx, owner, machine)
			return e
		})
	}
	if err := authorize(); err != nil {
		t.Fatalf("online machine: %v", err)
	}
	exec(`UPDATE user_machines SET state='disconnected',seat_state='released',online=false WHERE id=$1`, machine)
	if err := authorize(); !errors.Is(err, ErrMachineNotFound) {
		t.Fatalf("disconnected machine authorized as a vault host: %v", err)
	}
}
