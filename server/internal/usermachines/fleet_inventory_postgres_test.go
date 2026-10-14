package usermachines

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFleetUpdateInventoryPaginationOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	suffix := fmt.Sprint(time.Now().UnixNano())
	prefix := "fleet_inventory_" + suffix + "_"
	owner, foreign, team := prefix+"owner", prefix+"foreign", prefix+"team"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, command := range []struct {
			query string
			args  []any
		}{
			{`DELETE FROM paperboat.user_machine_update_observations WHERE user_machine_id IN(SELECT id FROM paperboat.user_machines WHERE user_id IN($1,$2))`, []any{owner, foreign}},
			{`DELETE FROM paperboat.user_machines WHERE user_id IN($1,$2)`, []any{owner, foreign}},
			{`DELETE FROM paperboat.team_members WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.teams WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.users WHERE id IN($1,$2)`, []any{owner, foreign}},
		} {
			if _, err := store.SQL().ExecContext(context.Background(), command.query, command.args...); err != nil {
				t.Errorf("fleet fixture cleanup: %v", err)
			}
		}
	})
	for _, actor := range []string{owner, foreign} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, actor, actor+"@example.test")
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, team, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,description,platform,architecture,workspace_root,state,seat_state)
 SELECT $1||lpad(n::text,3,'0'),$2,$1||'environment_'||n::text,'fleet-'||lpad(n::text,3,'0'),CASE WHEN n%5=0 THEN 'needle fixture' ELSE 'history fixture' END,'linux','amd64','/workspace','offline','released' FROM generate_series(1,75)n`, prefix, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,description,platform,architecture,workspace_root,state,seat_state,owner_team_id,deleted_at) VALUES
 ($1||'076',$2,$1||'environment_76','fleet-team-excluded','fixture','linux','amd64','/workspace','offline','released',$4,NULL),
 ($1||'077',$3,$1||'environment_77','fleet-foreign-excluded','fixture','linux','amd64','/workspace','offline','released',NULL,NULL),
 ($1||'078',$2,$1||'environment_78','fleet-deleted-excluded','fixture','linux','amd64','/workspace','deleted','released',NULL,now())`, prefix, owner, foreign, team)
	exec(`INSERT INTO paperboat.user_machine_update_observations(user_machine_id,environment_id,schema,current_version,target_version,channel,state,error_code,operation_id,installation_generation,worker_generation,os_boot_id,rollback_count,observed_at,payload_hash)
 SELECT m.id,m.environment_id,$2,'2026.10.08.34','2026.10.08.34','stable',CASE WHEN n%3=1 THEN 'healthy' ELSE 'failed' END,CASE WHEN n%3=1 THEN NULL ELSE 'fixture_failure' END,$1||'operation_'||n::text,1,1,'fixture-boot',0,now(),decode(repeat('00',32),'hex')
 FROM generate_series(1,75)n JOIN paperboat.user_machines m ON m.id=$1||lpad(n::text,3,'0') WHERE n%3<>0`, prefix, UpdateObservationSchemaV1)
	// Replacing the environment fences an otherwise healthy persisted observation.
	exec(`UPDATE paperboat.user_machines SET environment_id=$2 WHERE id=$1`, prefix+"073", prefix+"replacement_environment")
	service := New(store, nil, Policy{}, nil)
	first, err := service.FleetUpdateSummary(ctx, owner, InventoryOptions{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 50 || first.Pagination.Total != 75 || first.Pagination.Limit != 50 || first.Pagination.Offset != 0 || first.Pagination.NextOffset == nil || *first.Pagination.NextOffset != 50 || first.Counts["healthy"] != 24 || first.Counts["failed"] != 25 || first.Counts["not_reporting"] != 26 {
		t.Fatalf("first page size=%d pagination=%+v counts=%v", len(first.Items), first.Pagination, first.Counts)
	}
	second, err := service.FleetUpdateSummary(ctx, owner, InventoryOptions{Limit: 50, Offset: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 25 || second.Pagination.Total != 75 || second.Pagination.NextOffset != nil || second.Counts["not_reporting"] != 26 || second.Counts["healthy"] != 24 || second.Counts["failed"] != 25 {
		t.Fatalf("second page size=%d pagination=%+v counts=%v", len(second.Items), second.Pagination, second.Counts)
	}
	seen := map[string]bool{}
	for index, item := range append(first.Items, second.Items...) {
		expected := prefix + fmt.Sprintf("%03d", index+1)
		if item.MachineID != expected || seen[item.MachineID] {
			t.Fatalf("unstable or duplicate fleet page at %d: %s want %s", index, item.MachineID, expected)
		}
		seen[item.MachineID] = true
		if item.MachineID == prefix+"073" && (item.State != "not_reporting" || item.Observation != nil) {
			t.Fatal("replacement environment leaked stale observation")
		}
		if item.State == "not_reporting" && item.Observation != nil {
			t.Fatal("missing current observation fabricated reporting state")
		}
		if item.State != "not_reporting" && (item.Observation == nil || item.Observation.State != item.State || item.Observation.Schema != UpdateObservationSchemaV1) {
			t.Fatal("current observation projection mismatch")
		}
	}
	filtered, err := service.FleetUpdateSummary(ctx, owner, InventoryOptions{Limit: 2, Q: "NEEDLE", State: "healthy"})
	if err != nil || len(filtered.Items) != 2 || filtered.Pagination.Total != 5 || len(filtered.Counts) != 1 || filtered.Counts["healthy"] != 5 || filtered.Items[0].MachineID != prefix+"010" || filtered.Items[1].MachineID != prefix+"025" {
		t.Fatalf("filters before paging: %+v %v", filtered, err)
	}
	continuation, err := service.FleetUpdateSummary(ctx, owner, InventoryOptions{Limit: 2, Offset: 4, Q: "needle", State: "healthy"})
	if err != nil || len(continuation.Items) != 1 || continuation.Pagination.Total != 5 || continuation.Pagination.NextOffset != nil || continuation.Items[0].MachineID != prefix+"070" {
		t.Fatalf("filtered continuation: %+v %v", continuation, err)
	}
	missing, err := service.FleetUpdateSummary(ctx, owner, InventoryOptions{Limit: 1, State: "not_reporting", Q: prefix + "073"})
	if err != nil || missing.Pagination.Total != 1 || len(missing.Items) != 1 || missing.Items[0].Observation != nil || missing.Counts["not_reporting"] != 1 {
		t.Fatalf("exact changed environment state: %+v %v", missing, err)
	}
	other, err := service.FleetUpdateSummary(ctx, foreign, InventoryOptions{Limit: 50})
	if err != nil || other.Pagination.Total != 1 || len(other.Items) != 1 || other.Items[0].MachineID != prefix+"077" {
		t.Fatalf("foreign account inventory: %+v %v", other, err)
	}
	empty, err := service.FleetUpdateSummary(ctx, owner, InventoryOptions{Limit: 50, Offset: 75})
	if err != nil || empty.Items == nil || len(empty.Items) != 0 || empty.Pagination.Total != 75 || empty.Counts["not_reporting"] != 26 || empty.Pagination.NextOffset != nil {
		t.Fatalf("empty page lost totals: %+v %v", empty, err)
	}
	literal, err := service.FleetUpdateSummary(ctx, owner, InventoryOptions{Limit: 1, Q: "%"})
	if err != nil || literal.Pagination.Total != 0 || len(literal.Items) != 0 {
		t.Fatalf("q interpreted as wildcard: %+v %v", literal, err)
	}
	closed := true
	for _, options := range []InventoryOptions{{Limit: 0}, {Limit: 201}, {Limit: 1, Offset: -1}, {Limit: 1, Offset: 2147483648}, {Limit: 1, Q: strings.Repeat("x", 161)}, {Limit: 1, Q: "\x00"}, {Limit: 1, State: "offline"}, {Limit: 1, Owner: "mine"}, {Limit: 1, Closed: &closed}} {
		if _, err := service.FleetUpdateSummary(ctx, owner, options); !errors.Is(err, ErrUpdateObservationInvalid) {
			t.Fatalf("invalid fleet options accepted: %+v %v", options, err)
		}
	}
	if _, err := service.FleetUpdateSummary(ctx, "", InventoryOptions{Limit: 50}); !errors.Is(err, ErrUpdateObservationInvalid) {
		t.Fatalf("anonymous fleet read: %v", err)
	}
}
