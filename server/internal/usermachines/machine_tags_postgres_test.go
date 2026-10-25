package usermachines

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Machine items carry their tags from the selected workspace's policy file (the stored tags
// rows), so pb machine list and the console show and filter tags without reading the file.
func TestMachineItemsCarryTheirPolicyTagsOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner := "inv_tag_owner_" + suffix
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.workspace_policies WHERE owner_account=$1`, owner)
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
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) SELECT 'inv_tag_env_'||g||'_'||$1,'inv_tag_env_'||g||'_'||$1,$2 FROM generate_series(1,2) g`, suffix, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online)
 SELECT 'inv_tag_'||g||'_'||$1,$2,'inv_tag_env_'||g||'_'||$1,'tag-'||g,'linux','amd64','/w','online','occupied',true FROM generate_series(1,2) g`, suffix, owner)
	exec(`INSERT INTO paperboat.workspace_policies(owner_account,version,source,digest) VALUES($1,2,'{}','sha256:x')`, owner)
	exec(`INSERT INTO paperboat.workspace_policy_tags(policy_id,tag,machine_id) SELECT id,tag,'inv_tag_1_'||$2 FROM paperboat.workspace_policies, unnest(ARRAY['tag:ci','tag:a']) tag WHERE owner_account=$1`, owner, suffix)
	service := New(store, nil, Policy{}, nil)
	items, _, _, err := service.ListInventory(ctx, owner, InventoryOptions{Limit: 10})
	if err != nil || len(items) != 2 {
		t.Fatalf("inventory: %d items err=%v", len(items), err)
	}
	got := map[string]string{}
	for _, item := range items {
		if item.Tags == nil {
			t.Fatalf("%s: tags must be [] when it has none, not absent", item.Alias)
		}
		got[item.Alias] = strings.Join(item.Tags, ",")
	}
	if got["tag-1"] != "tag:a,tag:ci" || got["tag-2"] != "" {
		t.Fatalf("tags = %v", got)
	}
	one, err := service.Get(ctx, owner, "inv_tag_1_"+suffix)
	if err != nil || strings.Join(one.Tags, ",") != "tag:a,tag:ci" {
		t.Fatalf("get: %+v %v", one.Tags, err)
	}
}
