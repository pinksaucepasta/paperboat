package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// A team member's preview list and its total on a team machine shared with 20,000 previews: every row needs the
// host's policy check, which ran per row (about 2.5 ms each, 33 s for a total). Each host check runs once per host
// now (migration 174), so the total and a page take well under a few seconds.
func TestPreviewTotalOverTwentyThousandPreviewsOnPostgres(t *testing.T) {
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
	t.Cleanup(func() { _ = store.Close() }) // after the fixture cleanup below
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	owner, member, team, host := "pt_owner_"+s, "pt_member_"+s, "pt-team-"+s, "pt_host_"+s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active'),($2,$2,$2||'@example.test','active')`, owner, member)
	// Later tests on the same database (preview reconciliation scans every active lease) must not see these.
	t.Cleanup(func() {
		cleanup := context.Background()
		for _, query := range []string{
			`DELETE FROM paperboat.preview_leases WHERE owner_machine_id=$1`,
			`DELETE FROM paperboat.team_resource_bindings WHERE resource_id=$1`,
			`DELETE FROM paperboat.workspace_policies WHERE team_id=$2`,
			`DELETE FROM paperboat.team_members WHERE team_id=$2`,
			`DELETE FROM paperboat.billing_workspaces WHERE team_id=$2`,
			`DELETE FROM paperboat.user_machines WHERE id=$1`,
			`DELETE FROM paperboat.teams WHERE team_id=$2`,
			`DELETE FROM paperboat.users WHERE id IN ($3,$4)`,
		} {
			if _, err := store.SQL().ExecContext(cleanup, query, host, team, owner, member); err != nil {
				t.Errorf("cleanup: %v\n%s", err, query)
			}
		}
	})
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end)
VALUES($1,$2,'active','team',5,now()-interval '1 hour',now()+interval '1 hour')`, "bw_"+team, team)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'member',true)`, team, owner, member)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,
configured_capabilities,observed_capabilities) VALUES($1,$2,$1,'pt-host','linux','amd64','/w','online','occupied',true,ARRAY['preview_launch'],ARRAY['preview_launch'])`, host, owner)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation) VALUES($1,'machine',$2,$3,true,1)`, team, host, owner)
	exec(`INSERT INTO paperboat.workspace_policies(team_id,version,source,digest) VALUES($1,1,'{}','sha256:test')`, team)
	exec(`INSERT INTO paperboat.workspace_policy_rules(policy_id,rule,deny,subject_kind,subject_id,target_kind,target_id,action,ports)
SELECT id,0,false,'role','member','machine',$2,'preview_manage',int4range(1,65536) FROM paperboat.workspace_policies WHERE team_id=$1`, team, host)
	for from := 0; from < 20000; from += 5000 {
		exec(`INSERT INTO paperboat.preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,access_mode,
lease_deadline,owner_session_kind) SELECT 'pt_preview_'||g||'_'||$1,'https://pt'||g||'-'||$1||'.example.test',$2,$2,$3,'session','http','127.0.0.1:3000','team',
now()+interval '1 hour','local_lease' FROM generate_series($4::int,$4::int+4999) g`, s, owner, host, from)
	}
	exec(`ANALYZE paperboat.preview_leases`)

	started := time.Now()
	total, err := store.Queries().CountPreviewLeasesV1(ctx, dbsqlc.CountPreviewLeasesV1Params{AccountID: member})
	if err != nil || total != 20000 {
		t.Fatalf("total %d, %v; want the 20,000 previews the member may manage", total, err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("counting 20,000 manageable previews took %v", took)
	}
	started = time.Now()
	page, err := store.Queries().ListPreviewLeasesV1(ctx, dbsqlc.ListPreviewLeasesV1Params{AccountID: member, RowLimit: 51})
	if err != nil || len(page) != 51 {
		t.Fatalf("first page: %d rows, %v", len(page), err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the first page of 20,000 manageable previews took %v", took)
	}
	t.Logf("20,000 previews: total and first page in %v", time.Since(started))
}
