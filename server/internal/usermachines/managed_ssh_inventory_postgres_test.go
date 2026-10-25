package usermachines

import (
	"fmt"
	"testing"
	"time"
)

// The inventory carries each machine's managed SSH target, so daemons no longer fetch
// /ssh-target and /ssh-host-keys for every machine on every refresh. It appears only for the
// machine's current installation and only on machines the caller may still use.
func TestMachineInventoryCarriesManagedSSHTargetsOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner := "inv_ssh_owner_" + suffix
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.user_machines WHERE user_id=$1`, owner)
		_, _ = store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.users WHERE id=$1`, owner)
	})
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, owner, owner+"@example.test")
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) SELECT 'inv_ssh_env_'||g||'_'||$1,'inv_ssh_env_'||g||'_'||$1,$2 FROM generate_series(1,4) g`, suffix, owner)
	// 1 current target + active keys, 2 target from an older installation, 3 revoked, 4 target without keys.
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
 SELECT 'inv_ssh_'||g||'_'||$1,$2,'inv_ssh_env_'||g||'_'||$1,'ssh-'||g,'linux','amd64','/w',
  CASE WHEN g=3 THEN 'revoked' ELSE 'online' END, CASE WHEN g=3 THEN 'released' ELSE 'occupied' END, g<>3,
  CASE WHEN g=2 THEN 2 ELSE 1 END FROM generate_series(1,4) g`, suffix, owner)
	exec(`INSERT INTO paperboat.machine_ssh_targets(user_machine_id,machine_generation,os_user,target_port,created_at,updated_at)
 SELECT 'inv_ssh_'||g||'_'||$1,1,'pbt',2200+g,now(),now() FROM generate_series(1,4) g`, suffix)
	exec(`INSERT INTO paperboat.machine_ssh_host_key_sets(id,user_machine_id,machine_generation,observation_generation,set_fingerprint,state,reconciliation_version,observed_at,promoted_at)
 VALUES('keyset_'||gen_random_uuid()::text,'inv_ssh_1_'||$1,1,1,decode(repeat('ab',32),'hex'),'active',1,now(),now())`, suffix)
	service := New(store, nil, Policy{}, nil)
	items, _, _, err := service.ListInventory(ctx, owner, InventoryOptions{Limit: 10})
	if err != nil || len(items) != 3 {
		t.Fatalf("inventory: %d items err=%v; want the 3 machines in use", len(items), err)
	}
	// The default list leaves revoked machines out; asked for by state, one still carries no target.
	revoked, _, _, err := service.ListInventory(ctx, owner, InventoryOptions{Limit: 10, State: "revoked"})
	if err != nil || len(revoked) != 1 {
		t.Fatalf("revoked inventory: %d items err=%v", len(revoked), err)
	}
	items = append(items, revoked...)
	byAlias := map[string]*MachineSSH{}
	for _, item := range items {
		byAlias[item.Alias] = item.SSH
	}
	if got := byAlias["ssh-1"]; got == nil || *got != (MachineSSH{OSUser: "pbt", Port: 2201, TargetGeneration: 1, HostKeyGeneration: 1}) {
		t.Fatalf("current target with active host keys: %+v", got)
	}
	if got := byAlias["ssh-2"]; got != nil {
		t.Fatalf("a target from an older installation was listed: %+v", got)
	}
	if got := byAlias["ssh-3"]; got != nil {
		t.Fatalf("a revoked machine's target was listed: %+v", got)
	}
	if got := byAlias["ssh-4"]; got == nil || got.HostKeyGeneration != 0 || got.Port != 2204 {
		t.Fatalf("target without host keys: %+v", got)
	}
}
