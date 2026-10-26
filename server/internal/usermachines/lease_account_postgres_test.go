package usermachines

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/machineaddress"
)

// A local address lease belongs to an account (PLAN 3b "Multi-account hosts", expand step, migration 184): the
// allocator records the machine's owner, and until per-viewer leases exist another account that can see the
// machine (a team admin here) is shown the owner's address, exactly as before.
func TestLeasesBelongToTheirAccountOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	owner, admin, team, machine := "lease_owner_"+s, "lease_admin_"+s, "lease-team-"+s, "lease_machine_"+s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active'),($2,$2,$2||'@example.test','active')`, owner, admin)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end)
VALUES($1,$2,'active','team',5,now()-interval '1 hour',now()+interval '1 hour')`, "bw_"+team, team)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'admin',true)`, team, owner, admin)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,owner_team_id)
VALUES($1,$2,$1,'shared-host','linux','amd64','/w','online','occupied',true,$3)`, machine, owner, team)
	var slot int
	var account string
	exec(`SELECT paperboat.ensure_control_sync_lease($1)`, machine)
	if err := store.SQL().QueryRowContext(ctx, `SELECT address_slot,account_id FROM paperboat.control_sync_leases WHERE machine_id=$1`, machine).Scan(&slot, &account); err != nil || account != owner {
		t.Fatalf("lease of %s: account %q, %v; want the owner %s", machine, account, err, owner)
	}
	service := New(store, nil, Policy{}, nil)
	for _, viewer := range []string{owner, admin} {
		items, _, _, err := service.ListInventory(ctx, viewer, InventoryOptions{Limit: 10, Q: "shared-host"})
		if err != nil || len(items) != 1 || items[0].AssignedIP != machineaddress.FromSlot(slot).String() {
			t.Fatalf("%s's list: %+v, %v; want the machine at %s", viewer, items, err, machineaddress.FromSlot(slot))
		}
	}
}
