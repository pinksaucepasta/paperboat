package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func testOperatorRegionDelete(t *testing.T, store *db.DB,
	request func(string, string, string, bool, ...map[string]string) *httptest.ResponseRecorder,
	operatorID, accountID, regionID, nodeID string,
) {
	t.Helper()
	ctx := context.Background()
	path := "/v1/admin/regions/" + regionID
	key := "region-delete-" + accountID
	headers := map[string]string{"Operator-Target": regionID, "Operator-Reason": "http-delete", "Idempotency-Key": key}
	exec := func(t *testing.T, query string, args ...any) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	assertRefused := func(t *testing.T) {
		t.Helper()
		if r := request("DELETE", path, "", true, headers); r.Code != 409 || !strings.Contains(r.Body.String(), `"region_in_use"`) {
			t.Fatalf("referenced region delete = %d %s", r.Code, r.Body.String())
		}
	}
	t.Run("delete_region_without_confirmation", func(t *testing.T) {
		if r := request("DELETE", path, "", true); r.Code != 400 {
			t.Fatalf("unconfirmed region delete = %d %s", r.Code, r.Body.String())
		}
	})
	t.Run("delete_region_with_managed_node", assertRefused)
	t.Run("delete_region_with_pending_installation", func(t *testing.T) {
		exec(t, `UPDATE selfhost_installations SET claim_complete=false WHERE node_id=$1`, nodeID)
		assertRefused(t)
	})
	exec(t, `UPDATE control_tunnel_nodes SET state='retired',ready=false,drain_deadline=NULL WHERE id=$1`, nodeID)
	t.Run("delete_region_with_workspace_edge_set", func(t *testing.T) {
		exec(t, `INSERT INTO workspace_edge_sets(scope_kind,scope_id,mode,regions) VALUES('account',$1,'managed-only',ARRAY[$2::text])`, accountID, regionID)
		t.Cleanup(func() {
			exec(t, `DELETE FROM workspace_edge_sets WHERE scope_kind='account' AND scope_id=$1`, accountID)
		})
		assertRefused(t)
	})
	t.Run("delete_region_with_self_hosted_region", func(t *testing.T) {
		exec(t, `INSERT INTO selfhost_regions(scope_kind,scope_id,slug,display_name) VALUES('account',$1,$2,'Test region')`, accountID, regionID)
		t.Cleanup(func() {
			exec(t, `DELETE FROM selfhost_regions WHERE scope_kind='account' AND scope_id=$1 AND slug=$2`, accountID, regionID)
		})
		assertRefused(t)
	})
	t.Run("delete_unused_region_and_replay", func(t *testing.T) {
		deleted := request("DELETE", path, "", true, headers)
		if deleted.Code != 200 || !strings.Contains(deleted.Body.String(), regionID) {
			t.Fatalf("delete unused region = %d %s", deleted.Code, deleted.Body.String())
		}
		var exists bool
		if err := store.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_regions WHERE id=$1)`, regionID).Scan(&exists); err != nil || exists {
			t.Fatalf("region still exists: %v (%v)", exists, err)
		}
		replay := request("DELETE", path, "", true, headers)
		if replay.Code != 200 || replay.Body.String() != deleted.Body.String() {
			t.Fatalf("delete replay = %d %s", replay.Code, replay.Body.String())
		}
		var count int
		err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_events
WHERE actor_user_id=$1 AND resource_id=$2 AND event_type='operator.infrastructure.region.delete'
AND idempotency_key=$3 AND metadata->>'reason'='http-delete'`, operatorID, regionID, key).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("delete audit count = %d (%v)", count, err)
		}
	})
}
