package usermachines

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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
	keyA := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI" + strings.Repeat("A", 43)
	keyB := "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABB" + strings.Repeat("B", 60)
	exec(`INSERT INTO paperboat.machine_ssh_host_key_owners(fingerprint,user_machine_id,algorithm,public_key,first_observed_at) VALUES
 (decode(repeat('01',32),'hex'),'inv_ssh_1_'||$1,'ssh-ed25519',$2,now()),(decode(repeat('02',32),'hex'),'inv_ssh_1_'||$1,'ecdsa-sha2-nistp256',$3,now())`, suffix, keyA, keyB)
	setID := "keyset_" + uuid.NewString()
	topic := func() int64 {
		var generation int64
		_ = store.SQL().QueryRowContext(ctx, `SELECT coalesce((SELECT generation FROM paperboat.account_topic_generations WHERE account_id=$1 AND topic='inventory'),0)`, owner).Scan(&generation)
		return generation
	}
	exec(`INSERT INTO paperboat.machine_ssh_host_key_sets(id,user_machine_id,machine_generation,observation_generation,set_fingerprint,state,reconciliation_version,observed_at)
 VALUES($2,'inv_ssh_1_'||$1,1,1,decode(repeat('ab',32),'hex'),'pending',1,now())`, suffix, setID)
	exec(`INSERT INTO paperboat.machine_ssh_host_keys(set_id,user_machine_id,fingerprint,ordinal) VALUES
 ($2,'inv_ssh_1_'||$1,decode(repeat('02',32),'hex'),1),($2,'inv_ssh_1_'||$1,decode(repeat('01',32),'hex'),0)`, suffix, setID)
	before := topic()
	// A promotion (what pb ssh trust-host does) publishes the inventory topic, so daemons refetch the keys.
	exec(`UPDATE paperboat.machine_ssh_host_key_sets SET state='active',promoted_at=now() WHERE id=$1`, setID)
	if after := topic(); after <= before {
		t.Fatalf("inventory topic generation %d -> %d: a host key promotion did not publish it", before, after)
	}
	// ssh-4's keys were refused as ssh-1's (a cloned image): its listing says so, naming ssh-1 to the owner.
	exec(`INSERT INTO paperboat.machine_ssh_host_key_conflicts(user_machine_id,other_machine_id,observed_at) VALUES('inv_ssh_4_'||$1,'inv_ssh_1_'||$1,now())`, suffix)
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
	if got := byAlias["ssh-1"]; got == nil || got.OSUser != "pbt" || got.Port != 2201 || got.TargetGeneration != 1 || got.HostKeyGeneration != 1 ||
		!slices.Equal(got.HostKeys, []string{keyA, keyB}) || got.HostKeyConflict != nil {
		t.Fatalf("current target with active host keys: %+v, want the active set's keys in order", got)
	}
	if got := byAlias["ssh-2"]; got != nil {
		t.Fatalf("a target from an older installation was listed: %+v", got)
	}
	if got := byAlias["ssh-3"]; got != nil {
		t.Fatalf("a revoked machine's target was listed: %+v", got)
	}
	if got := byAlias["ssh-4"]; got == nil || got.HostKeyGeneration != 0 || got.Port != 2204 || len(got.HostKeys) != 0 ||
		got.HostKeyConflict == nil || got.HostKeyConflict.MachineName != "ssh-1" {
		t.Fatalf("target whose host keys were refused as ssh-1's: %+v", got)
	}
}
