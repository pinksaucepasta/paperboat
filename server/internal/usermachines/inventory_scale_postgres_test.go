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
		items, total, _, err := service.ListInventory(ctx, owner, InventoryOptions{Limit: 100})
		if err != nil || total != 100 || len(items) != 100 {
			t.Fatalf("inventory: %d of %d err=%v", len(items), total, err)
		}
		return items, time.Since(started)
	}
	// Pairing allocates each machine's local address lease (listing only reads it).
	if _, err := store.SQL().ExecContext(ctx, `SELECT paperboat.ensure_machine_address_leases($1, ARRAY(SELECT id FROM paperboat.user_machines WHERE user_id=$1))`, owner); err != nil {
		t.Fatal(err)
	}
	first, firstElapsed := list()
	if firstElapsed >= time.Second {
		t.Fatalf("first inventory of a fresh 100-machine account took %v, want under 1s", firstElapsed)
	}
	t.Logf("first list: %d machines in %v", len(first), firstElapsed)
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

// An unscoped list costs what the account's own candidates cost, not what every machine in the database costs: the
// planner once ran the access functions on every machine before applying the candidate list, 1 s per list on prod
// at ~300 machines in total (2026-11-12). Here a 10-machine account lists beside another account's 2,000 machines.
func TestMachineInventoryIgnoresOtherAccountsMachinesOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, crowd := "inv_fence_owner_"+suffix, "inv_fence_crowd_"+suffix
	t.Cleanup(func() {
		for _, account := range []string{owner, crowd} {
			_, _ = store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.user_machines WHERE user_id=$1`, account)
			_, _ = store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.control_environments WHERE owner_user_id=$1`, account)
			_, _ = store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.users WHERE id=$1`, account)
		}
	})
	for account, n := range map[string]int{owner: 10, crowd: 2000} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) SELECT $1||'_env_'||g,$1||'_env_'||g,$1 FROM generate_series(1,$2::int) g`, account, n); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
 SELECT $1||'_m_'||g,$1,$1||'_env_'||g,'machine-'||g,'linux','amd64','/w','online','occupied',true,1 FROM generate_series(1,$2::int) g`, account, n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SQL().ExecContext(ctx, `ANALYZE paperboat.user_machines`); err != nil {
		t.Fatal(err)
	}
	service := New(store, nil, Policy{}, nil)
	var elapsed time.Duration
	for range 3 { // the best of three: one slow run on a shared test host is not the plan
		started := time.Now()
		items, total, _, err := service.ListInventory(ctx, owner, InventoryOptions{Limit: 200})
		if err != nil || total != 10 || len(items) != 10 {
			t.Fatalf("inventory: %d of %d err=%v", len(items), total, err)
		}
		if run := time.Since(started); elapsed == 0 || run < elapsed {
			elapsed = run
		}
		time.Sleep(inventoryShareWindow) // a fresh computation each time, not the shared flight
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("listing 10 machines beside 2,000 others took %v, want under 200ms", elapsed)
	}
	t.Logf("10 machines beside 2,000 others: %v", elapsed)
}
