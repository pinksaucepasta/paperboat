package teams

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Authorizing one team preview read the whole team (every member, grant, machine and request, and the parsed
// policy) to check one membership and one grant: 3.1 s per decision on a seeded 200-member team, under the
// server-wide team lock. It must read only the caller's membership and that resource's rows, however large the
// team is, and decide exactly as before.
func TestAuthorizeReadsOnlyTheCallersMembershipAndTheResourceOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	owner, member, team, machine := "az_owner_"+s, "az_member_"+s, "az-team-"+s, "az_machine_"+s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	// A large team: 60 members, 40 machines and 120 previews with grants for every member.
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status)
SELECT x,x,x||'@example.test','active' FROM unnest(ARRAY[$1,$2]||ARRAY(SELECT 'az_other_'||g||'_'||$3 FROM generate_series(1,58) g)) x`, owner, member, s)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end)
VALUES($1,$2,'active','team',60,now()-interval '1 hour',now()+interval '1 hour')`, "bw_"+team, team)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active)
SELECT $1,id,1,'member',true FROM paperboat.users WHERE id=$2 OR id=$3 OR id LIKE 'az_other_%_'||$4`, team, owner, member, s)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities)
SELECT $1||'_'||g,$2,$1||'_'||g,'az-'||g,'linux','amd64','/w','online','occupied',true,ARRAY['preview_launch'] FROM generate_series(0,39) g`, machine, owner)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation)
SELECT $1,'machine',id,user_id,true,1 FROM paperboat.user_machines WHERE id LIKE $2||'_%'`, team, machine)
	exec(`INSERT INTO paperboat.preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,access_mode,
lease_deadline,owner_session_kind) SELECT 'az_preview_'||g||'_'||$3,'https://az'||g||'-'||$3||'.example.test',$1,$1,$2||'_'||(g%40),'session','http','127.0.0.1:3000','team',
now()+interval '1 hour','local_lease' FROM generate_series(0,119) g`, owner, machine, s)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation)
SELECT $1,'preview',id,account_id,true,1 FROM paperboat.preview_leases WHERE id LIKE 'az_preview_%_'||$2`, team, s)
	exec(`INSERT INTO paperboat.team_resource_grants(team_id,account_id,resource_kind,resource_id,permission,generation,active)
SELECT $1,m.account_id,'preview',p.id,'use',1,true FROM paperboat.team_members m, paperboat.preview_leases p
WHERE m.team_id=$1 AND m.account_id<>$2 AND p.id LIKE 'az_preview_%_'||$3`, team, owner, s)
	exec(`UPDATE paperboat.team_resource_grants SET active=false WHERE team_id=$1 AND account_id=$2 AND resource_id=$3`, team, member, "az_preview_1_"+s)

	var generation uint64
	if err := store.SQL().QueryRowContext(ctx, `SELECT generation FROM paperboat.teams WHERE team_id=$1`, team).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	var queries atomic.Int64
	store.SetQueryObserver(func(ctx context.Context) (context.Context, func(string)) {
		queries.Add(1)
		return ctx, func(string) {}
	})
	service := NewService(store)
	for _, c := range []struct {
		name, account, preview, permission string
		allowed                            bool
	}{
		{"granted use", member, "az_preview_0_" + s, "use", true},
		{"inactive grant", member, "az_preview_1_" + s, "use", false},
		{"not granted manage", member, "az_preview_0_" + s, "manage", false},
		{"outsider", "az_nobody_" + s, "az_preview_0_" + s, "use", false},
	} {
		queries.Store(0)
		d, err := service.Authorize(ctx, c.account, team, "preview", c.preview, c.permission)
		if c.allowed && (err != nil || d.GrantGeneration != 1 || d.MembershipGeneration != 1 || d.TeamGeneration != generation || d.BindingGeneration != 1) {
			t.Fatalf("%s: decision %+v, err %v; want allowed with the current generations", c.name, d, err)
		}
		if !c.allowed && err == nil {
			t.Fatalf("%s: allowed, want refused", c.name)
		}
		if n := queries.Load(); n > 10 {
			t.Fatalf("%s: %d queries for one decision, want at most 10 (the caller's membership and this preview's rows)", c.name, n)
		}
	}
}
