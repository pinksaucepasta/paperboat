package environment

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Removing a device must fence only the ENV sources that device could read.
// A machine that never received a layer must not push every other machine's
// delivery back to pending.
func TestRemovingANeverDeliveredMachineLeavesOtherDeliveriesFencedOnlyWhereItReadPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN for isolated vault fence transactions")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	setup, cancelSetup := context.WithTimeout(context.Background(), 5*time.Minute)
	err = db.Migrate(setup, store)
	cancelSetup()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	suffix := strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
	account := "fence_user_" + suffix
	source, recipient, junk := "fence_source_"+suffix, "fence_recipient_"+suffix, "fence_junk_"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$2,$3,'active')`, account, "workos_"+account, account+"@example.test")
	for _, id := range []string{source, recipient, junk} {
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'linux','amd64','/w','online','occupied',true,1)`, id, account, "env_"+id)
		exec(`INSERT INTO paperboat.environment_vault_hosts(machine_id,account_id,installation_generation,host_key_generation,host_public) VALUES($1,$2,1,1,$3)`, id, account, make([]byte, 32))
	}
	document := "sha256:" + strings.Repeat("a", 64)
	exec(`INSERT INTO paperboat.environment_vault_scopes(owner_kind,owner_id,machine_id,key_epoch,revision,document_id,envelope,writer_public,workspace_id) VALUES('personal',$1,$2,1,1,$3,'\x01',$4,'workspace')`, account, source, document, make([]byte, 32))
	exec(`INSERT INTO paperboat.environment_vault_layer_fences(workspace_id,owner_kind,owner_id,source_machine_id,generation) VALUES('workspace','personal',$1,$2,1)`, account, source)
	exec(`INSERT INTO paperboat.environment_vault_layers(machine_id,workspace_id,owner_kind,owner_id,source_machine_id,delivery_generation,document_id,installation_generation,host_key_generation,applied_fence_generation,writer_account,writer_public,envelope) VALUES($1,'workspace','personal',$2,$3,1,$4,1,1,1,$2,$5,'\x01')`, recipient, account, source, document, make([]byte, 32))
	fence := func() int64 {
		var generation int64
		if err := store.SQL().QueryRowContext(ctx, `SELECT generation FROM paperboat.environment_vault_layer_fences WHERE owner_id=$1 AND source_machine_id=$2`, account, source).Scan(&generation); err != nil {
			t.Fatal(err)
		}
		return generation
	}
	remove := func(machine string) {
		t.Helper()
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			return db.FenceRemovedMachineVaultTx(ctx, tx, account, machine)
		}); err != nil {
			t.Fatal(err)
		}
	}
	remove(junk)
	if got := fence(); got != 1 {
		t.Fatalf("removing a machine that never received ENV advanced the fence to %d", got)
	}
	remove(recipient)
	if got := fence(); got != 2 {
		t.Fatalf("removing a machine that held a layer left the fence at %d, want 2", got)
	}
	remove("")
	if got := fence(); got != 3 {
		t.Fatalf("an account-wide fence (revoked CLI session) left the fence at %d, want 3", got)
	}
}
