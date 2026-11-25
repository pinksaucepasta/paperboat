package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// Migration 519 rewrote how the preview list decides, not what it decides: the functions of 174 and 210, installed
// beside it under other names, must give the same previews, order and totals for every actor, scope, filter and page,
// before and after memberships, teams and machines are withdrawn.
func TestPreviewListMatchesThePreviousDecisionsOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	oracleFunction := func(file, create, name string) string {
		t.Helper()
		text, err := os.ReadFile("migrations/" + file)
		if err != nil {
			t.Fatal(err)
		}
		_, body, found := strings.Cut(string(text), create+" FUNCTION paperboat."+name+"(")
		body, _, ended := strings.Cut(body, "\n$$;")
		if !found || !ended {
			t.Fatalf("%s does not define %s", file, name)
		}
		return strings.NewReplacer("preview_management_decision", "oracle_preview_decision", "manageable_preview_ids", "oracle_preview_ids").Replace(
			"CREATE OR REPLACE FUNCTION paperboat." + name + "(" + body + "\n$$;")
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DROP FUNCTION IF EXISTS
 paperboat.oracle_preview_ids(text,boolean,text,text,text,text,text,timestamptz,text,integer),
 paperboat.oracle_preview_decision(text,text,text,text,text,text,text,text[],boolean,boolean),
 paperboat.workspace_oracle_preview_decision(text,text,text,text,text,text,text,text,text[],boolean)`)
	})
	exec(oracleFunction("174_publication_access_decisions.sql", "CREATE", "workspace_preview_management_decision"))
	exec(oracleFunction("174_publication_access_decisions.sql", "CREATE", "preview_management_decision"))
	exec(oracleFunction("210_publications_exclude_removed_machines.sql", "CREATE OR REPLACE", "manageable_preview_ids"))

	f := newPublicationFixture(t, ctx, store)
	owner, member, outsider := f.users[0], f.users[2], f.users[4]
	// Every lifecycle state, and pairs of previews created at the same instant (the id breaks the tie).
	exec(`WITH n AS (SELECT id, row_number() OVER (ORDER BY id) AS k FROM paperboat.preview_leases WHERE id LIKE 'pc_preview_%_'||$1)
UPDATE paperboat.preview_leases p SET created_at=now()-(n.k/2)*interval '1 second',
 terminal_state=CASE n.k%7 WHEN 1 THEN 'stopped' WHEN 2 THEN 'expired' WHEN 3 THEN 'owner_lost' WHEN 4 THEN 'failed' ELSE 'active' END,
 stopped_at=CASE WHEN n.k%7 BETWEEN 1 AND 4 THEN now() END,
 allocation_state=CASE WHEN n.k%7=5 THEN 'ready' ELSE p.allocation_state END,
 edge_state=CASE WHEN n.k%14=5 THEN 'ready' ELSE p.edge_state END, origin_state=CASE WHEN n.k%14=5 THEN 'ready' ELSE p.origin_state END
FROM n WHERE n.id=p.id`, f.s)

	seen := map[string]int{}
	compare := func(stage string, arg dbsqlc.ListPreviewLeasesV1Params) []dbsqlc.PreviewLease {
		t.Helper()
		oracle := `SELECT c.id FROM paperboat.preview_leases c WHERE c.id IN (SELECT paperboat.oracle_preview_ids($1,$2,$3,$4,$5,$6,$7,$8,$9,$10))
ORDER BY c.created_at DESC, c.id DESC`
		read := func(after sql.NullTime, id sql.NullString, limit sql.NullInt32) []string {
			t.Helper()
			rows, err := store.SQL().QueryContext(ctx, oracle, arg.AccountID, arg.WorkspaceSet, arg.WorkspaceTeamID, arg.Q, arg.State, arg.Owner, arg.MachineID, after, id, limit)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var ids []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			return ids
		}
		rows, err := store.Queries().ListPreviewLeasesV1(ctx, arg)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, row := range rows {
			got = append(got, row.ID)
		}
		if want := read(arg.AfterCreatedAt, arg.AfterID, sql.NullInt32{Int32: arg.RowLimit, Valid: true}); !slices.Equal(got, want) {
			t.Fatalf("%s %+v: listed %v, previously %v", stage, arg, got, want)
		}
		total, err := store.Queries().CountPreviewLeasesV1(ctx, dbsqlc.CountPreviewLeasesV1Params{AccountID: arg.AccountID, WorkspaceSet: arg.WorkspaceSet,
			WorkspaceTeamID: arg.WorkspaceTeamID, Q: arg.Q, State: arg.State, Owner: arg.Owner, MachineID: arg.MachineID})
		if err != nil {
			t.Fatal(err)
		}
		if want := len(read(sql.NullTime{}, sql.NullString{}, sql.NullInt32{})); total != int64(want) {
			t.Fatalf("%s %+v: total %d, previously %d", stage, arg, total, want)
		}
		return rows
	}
	sweep := func(stage string) {
		t.Helper()
		for _, actor := range append(slices.Clone(f.users), "pc_nobody_"+f.s) {
			for _, scope := range []dbsqlc.ListPreviewLeasesV1Params{{}, {WorkspaceSet: true},
				{WorkspaceSet: true, WorkspaceTeamID: sql.NullString{String: f.team, Valid: true}},
				{WorkspaceSet: true, WorkspaceTeamID: sql.NullString{String: "pc-other-" + f.s, Valid: true}}} {
				scope.AccountID, scope.Q, scope.RowLimit = actor, f.s, 200
				all := compare(stage, scope)
				seen[stage] += len(all)
				for _, limit := range []int32{1, 3} {
					page := scope
					page.RowLimit = limit
					compare(stage, page)
				}
				for _, row := range all { // the cursor at every row, the last one included (past the end)
					page := scope
					page.RowLimit = 3
					page.AfterCreatedAt, page.AfterID = sql.NullTime{Time: row.CreatedAt, Valid: true}, sql.NullString{String: row.ID, Valid: true}
					compare(stage, page)
				}
				for _, state := range []string{"allocating", "connecting", "ready", "owner_disconnected", "expired", "stopped", "active", "ended"} {
					filtered := scope
					filtered.State = state
					seen["state "+state] += len(compare(stage, filtered))
				}
				for _, by := range []string{"mine", "shared", owner, outsider} {
					filtered := scope
					filtered.Owner = by
					seen["owner"] += len(compare(stage, filtered))
				}
				for _, machine := range append(slices.Clone(f.machines), "pc_machine_absent_"+f.s) {
					filtered := scope
					filtered.MachineID = machine
					seen["machine"] += len(compare(stage, filtered))
				}
				for _, q := range []string{"", "PC_PREVIEW_1", "no-such-preview"} {
					filtered := scope
					filtered.Q = q
					compare(stage, filtered)
				}
			}
		}
	}
	sweep("initial")
	exec(`UPDATE paperboat.teams SET suspended=true,suspended_at=now(),suspended_by=$2 WHERE team_id=$1`, f.team, owner)
	sweep("suspended team")
	exec(`UPDATE paperboat.teams SET suspended=false,suspended_at=NULL,suspended_by=NULL WHERE team_id=$1`, f.team)
	exec(`UPDATE paperboat.billing_workspaces SET state='unpaid' WHERE team_id=$1`, f.team)
	sweep("unpaid team")
	exec(`UPDATE paperboat.billing_workspaces SET state='active' WHERE team_id=$1`, f.team)
	exec(`UPDATE paperboat.team_members SET active=false WHERE team_id=$1 AND account_id=$2`, f.team, member)
	sweep("removed member")
	exec(`UPDATE paperboat.user_machines SET configured_capabilities=ARRAY['terminal_host'] WHERE id=$1`, "pc_machine_team_picked_"+f.s)
	exec(`UPDATE paperboat.user_machines SET revoked_at=now() WHERE id=$1`, "pc_machine_shared_"+f.s)
	sweep("revoked machine")
	exec(`UPDATE paperboat.user_machines SET deleted_at=now(),state='deleted' WHERE id=$1`, "pc_machine_member_own_"+f.s)
	sweep("deleted machine")
	for _, part := range []string{"initial", "suspended team", "unpaid team", "removed member", "revoked machine", "deleted machine", "owner", "machine",
		"state allocating", "state connecting", "state ready", "state owner_disconnected", "state expired", "state stopped", "state active", "state ended"} {
		if seen[part] == 0 {
			t.Errorf("fixture too weak: nothing listed for %s", part)
		}
	}
	t.Log(fmt.Sprint(seen))
}
