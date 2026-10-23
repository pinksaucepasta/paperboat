package usermachines

import (
	"fmt"
	"testing"
	"time"
)

// An inventory page loads its machines and their access description in a fixed number of queries, whatever the
// page size: 100 machines (a third of them revoked/offline, as a long-lived test account collects) list well under
// 300 ms. The per-machine Get it used to run per row took seconds.
func TestMachineInventoryListsAHundredMachinesWithoutPerRowQueriesOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner := "inv_scale_owner_" + suffix
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.user_machines WHERE user_id=$1`, owner)
		_, _ = store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.users WHERE id=$1`, owner)
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, owner, owner+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) SELECT 'inv_env_'||g||'_'||$1,'inv_env_'||g||'_'||$1,$2 FROM generate_series(1,100) g`, suffix, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
 SELECT 'inv_machine_'||g||'_'||$1,$2,'inv_env_'||g||'_'||$1,'machine-'||g,'linux','amd64','/w',
  CASE WHEN g%3=0 THEN 'pending' WHEN g%3=1 THEN 'online' ELSE 'offline' END,
  CASE WHEN g%3=0 THEN 'released' ELSE 'occupied' END, g%3=1, 1 FROM generate_series(1,100) g`, suffix, owner); err != nil {
		t.Fatal(err)
	}
	service := New(store, nil, Policy{}, nil)
	list := func() ([]UserMachine, time.Duration) {
		started := time.Now()
		items, total, err := service.ListInventory(ctx, owner, InventoryOptions{Limit: 100})
		if err != nil || total != 100 || len(items) != 100 {
			t.Fatalf("inventory: %d of %d err=%v", len(items), total, err)
		}
		return items, time.Since(started)
	}
	// Each machine's control sync connection allocates its local address lease (listing only reads it).
	if _, err := store.SQL().ExecContext(ctx, `SELECT paperboat.ensure_control_sync_lease(id) FROM paperboat.user_machines WHERE user_id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	list()
	items, elapsed := list()
	if elapsed > 300*time.Millisecond {
		t.Fatalf("listing 100 machines took %v, want under 300ms", elapsed)
	}
	for _, item := range items {
		if !item.CanManage || len(item.Permissions) == 0 || item.AssignedIP == "" {
			t.Fatalf("machine %s lost its access description: manage=%v permissions=%v ip=%q", item.ID, item.CanManage, item.Permissions, item.AssignedIP)
		}
	}
}
