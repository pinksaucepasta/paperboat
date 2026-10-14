package usermachines

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceApprovalInventoryPaginationOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, foreign := "usr_maintenance_inventory_"+suffix, "usr_maintenance_foreign_"+suffix
	machine := "mch_maintenance_inventory_" + suffix
	prefix := "approval_inventory_" + suffix + "_"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		for _, command := range []struct {
			query string
			args  []any
		}{
			{`DELETE FROM paperboat.user_machine_maintenance_approvals WHERE user_machine_id=$1`, []any{machine}},
			{`DELETE FROM paperboat.user_machines WHERE id=$1`, []any{machine}},
			{`DELETE FROM paperboat.users WHERE id IN($1,$2)`, []any{owner, foreign}},
		} {
			if _, err := store.SQL().ExecContext(cleanup, command.query, command.args...); err != nil {
				t.Errorf("maintenance fixture cleanup: %v", err)
			}
		}
	})
	for _, actor := range []string{owner, foreign} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, actor, actor+"@example.test")
	}
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'maintenance-inventory','linux','amd64','/workspace','offline','released')`, machine, owner, "env_maintenance_inventory_"+suffix)
	// One statement deliberately gives every history row the same created_at;
	// the exact descending ID tie-break must survive the page boundary.
	exec(`INSERT INTO paperboat.user_machine_maintenance_approvals(id,user_machine_id,user_id,schema,action,target_version,reason,status,idempotency_key,request_hash,expires_at,created_at)
 SELECT $1||lpad(n::text,3,'0'),$2,$3,$4,'update','2026.10.08.34',
 CASE WHEN n%5=0 THEN 'needle fixture' ELSE 'history fixture' END,
 CASE WHEN n%3=0 THEN 'pending' WHEN n%3=1 THEN 'approved' ELSE 'rejected' END,
 $1||lpad(n::text,3,'0'),decode(repeat('00',32),'hex'),
 CASE WHEN n=75 THEN now()-interval '1 hour' ELSE now()+interval '1 day' END,now()-interval '1 day'
 FROM generate_series(1,75)n`, prefix, machine, owner, MaintenanceApprovalSchemaV1)
	service := New(store, nil, Policy{}, nil)
	if _, _, err := service.ListMaintenanceApprovals(ctx, foreign, machine, InventoryOptions{Limit: 50}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign history read: %v", err)
	}
	var status string
	if err := store.SQL().QueryRowContext(ctx, `SELECT status FROM paperboat.user_machine_maintenance_approvals WHERE id=$1`, prefix+"075").Scan(&status); err != nil || status != "pending" {
		t.Fatalf("foreign read changed pending approval: %s %v", status, err)
	}
	first, total, err := service.ListMaintenanceApprovals(ctx, owner, machine, InventoryOptions{Limit: 50})
	if err != nil || total != 75 || len(first) != 50 {
		t.Fatalf("first page rows=%d total=%d: %v", len(first), total, err)
	}
	second, total, err := service.ListMaintenanceApprovals(ctx, owner, machine, InventoryOptions{Limit: 50, Offset: 50})
	if err != nil || total != 75 || len(second) != 25 {
		t.Fatalf("second page rows=%d total=%d: %v", len(second), total, err)
	}
	seen := map[string]bool{}
	for index, row := range append(first, second...) {
		expected := prefix + fmt.Sprintf("%03d", 75-index)
		if row.ID != expected || seen[row.ID] || !row.CreatedAt.Equal(first[0].CreatedAt) {
			t.Fatalf("unstable or duplicate tied history at %d: %s want %s", index, row.ID, expected)
		}
		seen[row.ID] = true
	}
	if first[0].Status != "expired" || first[0].Version != 2 {
		t.Fatalf("due pending history did not transition once: status=%s version=%d", first[0].Status, first[0].Version)
	}
	for _, test := range []struct {
		state string
		total int
	}{{"pending", 24}, {"approved", 25}, {"rejected", 25}, {"expired", 1}, {"consumed", 0}} {
		rows, total, err := service.ListMaintenanceApprovals(ctx, owner, machine, InventoryOptions{Limit: 1, State: test.state})
		wantRows := 1
		if test.total == 0 {
			wantRows = 0
		}
		if err != nil || total != test.total || len(rows) != wantRows {
			t.Fatalf("state %s rows=%d total=%d: %v", test.state, len(rows), total, err)
		}
		if len(rows) > 0 && rows[0].Status != test.state {
			t.Fatal("state filter applied after page limit")
		}
	}
	rows, total, err := service.ListMaintenanceApprovals(ctx, owner, machine, InventoryOptions{Limit: 2, Q: "NEEDLE", State: "approved"})
	if err != nil || total != 5 || len(rows) != 2 || rows[0].ID != prefix+"070" || rows[1].ID != prefix+"055" {
		t.Fatalf("search/state before limit rows=%v total=%d: %v", rows, total, err)
	}
	rows, total, err = service.ListMaintenanceApprovals(ctx, owner, machine, InventoryOptions{Limit: 2, Q: "NEEDLE", State: "approved", Offset: 4})
	if err != nil || total != 5 || len(rows) != 1 || rows[0].ID != prefix+"010" {
		t.Fatalf("filtered continuation rows=%v total=%d: %v", rows, total, err)
	}
	rows, total, err = service.ListMaintenanceApprovals(ctx, owner, machine, InventoryOptions{Limit: 50, Offset: 75})
	if err != nil || total != 75 || rows == nil || len(rows) != 0 {
		t.Fatalf("empty page total lost: rows=%v total=%d: %v", rows, total, err)
	}
	rows, total, err = service.ListMaintenanceApprovals(ctx, owner, machine, InventoryOptions{Limit: 1, Q: "%"})
	if err != nil || total != 0 || len(rows) != 0 {
		t.Fatalf("literal search became wildcard: %d %v", total, err)
	}
	rows, total, err = service.ListMaintenanceApprovals(ctx, owner, machine, InventoryOptions{Limit: 1, State: "expired"})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].Version != 2 {
		t.Fatalf("repeat read repeated expiry mutation: %v %d %v", rows, total, err)
	}
	closed := true
	for _, options := range []InventoryOptions{{Limit: 0}, {Limit: 201}, {Limit: 1, Offset: -1}, {Limit: 1, Offset: 2147483648}, {Limit: 1, Q: strings.Repeat("x", 161)}, {Limit: 1, Q: "\x00"}, {Limit: 1, State: "unknown"}, {Limit: 1, Owner: "mine"}, {Limit: 1, Closed: &closed}} {
		if _, _, err := service.ListMaintenanceApprovals(ctx, owner, machine, options); !errors.Is(err, ErrMaintenanceApprovalInvalid) {
			t.Fatalf("invalid options accepted: %+v %v", options, err)
		}
	}
}
