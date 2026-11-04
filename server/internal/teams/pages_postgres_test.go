package teams

import (
	"context"
	"reflect"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// F21-42: the member, grant and machine pages walk the same rows, in the same order, as the full read a write
// authorizes with; their totals are the team's counts; a cursor of one list pages no other.
func TestPostgresTeamPagesEqualTheFullRead(t *testing.T) {
	f := newPolicyFixture(t)
	f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT $1||g,$1||g,$1||g||'@example.test','active' FROM generate_series(1,250) g`, f.team+"_m")
	f.exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) SELECT $1,$2||g,1,'member',true FROM generate_series(1,250) g`, f.team, f.team+"_m")
	t.Cleanup(func() {
		_, _ = f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.team_resource_grants WHERE team_id=$1`, f.team)
		_, _ = f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.team_resource_bindings WHERE team_id=$1 AND resource_kind IN ('preview','tunnel')`, f.team)
		_, _ = f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.team_members WHERE account_id LIKE $1`, f.team+"_m%")
		_, _ = f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id LIKE $1`, f.team+"_m%")
	})
	f.exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) SELECT $1,k,k||'_'||r,$2 FROM unnest(ARRAY['preview','tunnel']) k, generate_series(1,2) r`, f.team, f.owner)
	f.exec(`INSERT INTO paperboat.team_resource_grants(team_id,account_id,resource_kind,resource_id,permission,generation,active) SELECT $1,a,k,k||'_'||r,'use',1,true FROM unnest($2::text[]) a, unnest(ARRAY['preview','tunnel']) k, generate_series(1,2) r`, f.team, []string{f.member, f.other, f.admin})
	var generation uint64
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT generation FROM paperboat.teams WHERE team_id=$1`, f.team).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Grant(f.ctx, f.owner, f.team, GrantRequest{OperationID: "pages_env", ExpectedGeneration: generation, AccountID: f.member, ResourceKind: "env", ResourceID: f.team, Permission: "read", Active: true}); err != nil {
		t.Fatal(err)
	}
	var full Team
	if err := f.store.InReadOnlySnapshot(f.ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		full, err = readTx(ctx, tx, f.team, "", false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := f.service.Get(f.ctx, f.owner, f.team)
	if err != nil {
		t.Fatal(err)
	}
	if summary.MemberCount != len(full.Members) || summary.GrantCount != len(full.Grants) || summary.MachineCount != len(full.Machines) || len(summary.Members) != 0 || summary.MyRole != "owner" {
		t.Fatalf("summary counts %d/%d/%d role %q, full %d/%d/%d", summary.MemberCount, summary.GrantCount, summary.MachineCount, summary.MyRole, len(full.Members), len(full.Grants), len(full.Machines))
	}
	if len(full.Members) < 250 || len(full.Grants) < 13 || len(full.Machines) != 4 {
		t.Fatalf("fixture: %d members, %d grants, %d machines", len(full.Members), len(full.Grants), len(full.Machines))
	}
	walk := func(name string, limit int, read func(cursor string) (any, int, string, error)) []any {
		t.Helper()
		var all []any
		cursor := ""
		for pages := 0; ; pages++ {
			items, total, next, err := read(cursor)
			if err != nil {
				t.Fatalf("%s page: %v", name, err)
			}
			if pages > 400 {
				t.Fatalf("%s never ends", name)
			}
			rows := reflect.ValueOf(items)
			for i := range rows.Len() {
				all = append(all, rows.Index(i).Interface())
			}
			if total != len(all) && next == "" || rows.Len() > limit {
				t.Fatalf("%s: total %d, walked %d, page %d > %d", name, total, len(all), rows.Len(), limit)
			}
			if next == "" {
				return all
			}
			cursor = next
		}
	}
	same := func(name string, got []any, want any) {
		t.Helper()
		w := reflect.ValueOf(want)
		if len(got) != w.Len() {
			t.Fatalf("%s: paged %d rows, full read %d", name, len(got), w.Len())
		}
		for i := range got {
			if !reflect.DeepEqual(got[i], w.Index(i).Interface()) {
				t.Fatalf("%s row %d: paged %+v, full %+v", name, i, got[i], w.Index(i).Interface())
			}
		}
	}
	same("members", walk("members", 100, func(c string) (any, int, string, error) {
		p, err := f.service.Members(f.ctx, f.owner, f.team, c, 100)
		return p.Items, p.Total, p.NextCursor, err
	}), full.Members)
	same("machines", walk("machines", 3, func(c string) (any, int, string, error) {
		p, err := f.service.Machines(f.ctx, f.owner, f.team, c, 3)
		return p.Items, p.Total, p.NextCursor, err
	}), full.Machines)
	for _, limit := range []int{1, 4, 200} {
		same("grants", walk("grants", limit, func(c string) (any, int, string, error) {
			p, err := f.service.Grants(f.ctx, f.owner, f.team, c, limit)
			return p.Items, p.Total, p.NextCursor, err
		}), full.Grants)
	}
	first, err := f.service.Members(f.ctx, f.owner, f.team, "", 1)
	if err != nil || first.NextCursor == "" || first.Total != len(full.Members) {
		t.Fatalf("first member page %+v, %v", first, err)
	}
	if _, err := f.service.Machines(f.ctx, f.owner, f.team, first.NextCursor, 1); err != ErrInvalid {
		t.Fatalf("a members cursor paged machines: %v", err)
	}
	if _, err := f.service.Members(f.ctx, f.owner, f.team, "", MaximumPageLimit+1); err != ErrInvalid {
		t.Fatalf("limit over the maximum: %v", err)
	}
	if _, err := f.service.Members(f.ctx, "stranger_"+f.team, f.team, "", 0); err != ErrForbidden {
		t.Fatalf("a stranger read members: %v", err)
	}
}
