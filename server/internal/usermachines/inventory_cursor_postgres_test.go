package usermachines

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// The console pages machines by keyset cursor instead of loading every machine (Lane C, PQ-04 C1): walking the
// cursor lists exactly the full list, every page carries the same total, and platform, tag, state=revoked and
// an address in q select what they name.
func TestMachineListCursorAndFiltersOnPostgres(t *testing.T) {
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
	user := fmt.Sprintf("cursor_user_%d", time.Now().UnixNano())
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	// Seven machines in use whose alias order differs from their id order, one revoked, one disconnected (both removed).
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online)
SELECT $1||'_m'||g,$1,$1||'_e'||g,(ARRAY['bravo','alpha','delta','charlie','echo','foxtrot','golf','hotel','india'])[g],
 CASE WHEN g%3=0 THEN 'darwin' ELSE 'linux' END,'amd64','/w',
 CASE WHEN g=8 THEN 'revoked' WHEN g=9 THEN 'disconnected' WHEN g%2=0 THEN 'online' ELSE 'offline' END,
 CASE WHEN g>=8 THEN 'released' ELSE 'occupied' END,g%2=0 FROM generate_series(1,9) g`, user)
	exec(`UPDATE paperboat.user_machines SET revoked_at=now() WHERE id=$1`, user+"_m8")
	exec(`INSERT INTO paperboat.workspace_policies(owner_account,version,source,digest) VALUES($1,1,'{}','sha256:test')`, user)
	exec(`INSERT INTO paperboat.workspace_policy_tags(policy_id,tag,machine_id) SELECT id,'tag:prod',$1||'_m'||g FROM paperboat.workspace_policies,generate_series(2,3) g WHERE owner_account=$1`, user)
	exec(`SELECT paperboat.ensure_machine_address_leases((SELECT user_id FROM paperboat.user_machines WHERE id=$1), ARRAY[$1]::text[])`, user+"_m5")
	var address string
	if err := store.SQL().QueryRowContext(ctx, `SELECT '127.100.'||(address_slot/256)||'.'||(address_slot%256) FROM paperboat.control_sync_leases WHERE machine_id=$1`, user+"_m5").Scan(&address); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO paperboat.user_machine_update_observations(user_machine_id,environment_id,schema,current_version,target_version,channel,state,error_code,operation_id,installation_generation,worker_generation,os_boot_id,rollback_count,observed_at,payload_hash)
 SELECT id,environment_id,$2,'2026.10.25.1','2026.10.25.1','stable','healthy',NULL,$1||'_op',1,1,'boot',0,now(),decode(repeat('00',32),'hex') FROM paperboat.user_machines WHERE id=$1||'_m1'`, user, UpdateObservationSchemaV1)
	service := New(store, nil, Policy{}, nil)
	personal := workspace.WithContext(ctx, workspace.Scope{ID: "personal"})
	ids := func(items []UserMachine) []string {
		out := make([]string, len(items))
		for i, item := range items {
			out[i] = strings.TrimPrefix(item.ID, user+"_")
		}
		return out
	}
	full, total, more, err := service.ListInventory(personal, user, InventoryOptions{Limit: 200})
	if err != nil || total != 7 || more {
		t.Fatalf("full list: %d items, total %d, more %v, %v", len(full), total, more, err)
	}
	if got := ids(full); !slices.Equal(got, []string{"m2", "m1", "m4", "m3", "m5", "m6", "m7"}) {
		t.Fatalf("full list order %v, want alias order", got)
	}
	// Each item carries its update observation (the Machines page's version column), null before a report.
	for _, item := range full {
		reported := item.ID == user+"_m1"
		if (item.Update != nil) != reported || (reported && item.Update.CurrentVersion != "2026.10.25.1") {
			t.Fatalf("machine %s update observation %+v", item.ID, item.Update)
		}
	}
	// The machine page reads the same observation from GET /v1/machines/{id}.
	if one, err := service.Get(personal, user, user+"_m1"); err != nil || one.Update == nil || one.Update.CurrentVersion != "2026.10.25.1" {
		t.Fatalf("single machine update observation %+v, %v", one.Update, err)
	}
	var walked []string
	opts := InventoryOptions{Limit: 2}
	for page := 0; ; page++ {
		items, pageTotal, more, err := service.ListInventory(personal, user, opts)
		if err != nil || pageTotal != 7 {
			t.Fatalf("page %d: total %d, %v; want 7 on every page", page, pageTotal, err)
		}
		walked = append(walked, ids(items)...)
		if !more {
			break
		}
		last := items[len(items)-1]
		opts.After = &InventoryPosition{Key: strings.ToLower(last.Alias), ID: last.ID}
	}
	if !slices.Equal(walked, ids(full)) {
		t.Fatalf("walking the cursor listed %v, the full list is %v", walked, ids(full))
	}
	for _, c := range []struct {
		name string
		opts InventoryOptions
		want []string
	}{
		{"platform", InventoryOptions{Limit: 50, Platform: "darwin"}, []string{"m3", "m6"}},
		{"tag", InventoryOptions{Limit: 50, Tag: "prod"}, []string{"m2", "m3"}},
		{"revoked and disconnected are gone", InventoryOptions{Limit: 50, State: "revoked"}, nil},
		{"address", InventoryOptions{Limit: 50, Q: address}, []string{"m5"}},
		{"hostname", InventoryOptions{Limit: 50, Q: "Echo.example.test"}, []string{"m5"}},
	} {
		items, total, _, err := service.ListInventory(personal, user, c.opts)
		if got := ids(items); err != nil || !slices.Equal(got, c.want) || total != len(c.want) {
			t.Errorf("%s: %v (total %d), %v; want %v", c.name, got, total, err, c.want)
		}
	}
}
