package machineservices_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/machineservices"
)

// The list's set query and ResolveTx are two statements of one rule: over random memberships, roles, SSO, billing,
// suspension, bindings, rules, tags, machines, lazy policies and tunnels, the list shows exactly the announced ports
// ResolveTx grants.
func TestListMatchesResolveTxOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	store, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if os.Getenv("PAPERBOAT_TEST_SCHEMA_READY") != "1" {
		if e = db.Migrate(context.Background(), store); e != nil {
			t.Fatal(e)
		}
	}
	// Fixture rows are written without triggers and foreign keys, to reach states the product only passes through.
	fixture, e := store.SQL().Conn(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer fixture.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := fixture.ExecContext(ctx, q, args...); e != nil {
			t.Fatalf("%s: %v", q, e)
		}
	}
	exec(`SET session_replication_role=replica`)
	defer exec(`SET session_replication_role=DEFAULT`)
	exec(`SELECT setseed(0.42)`)
	x := fmt.Sprintf("la%d-", time.Now().UnixNano())
	id := func(kind string, i int) string { return fmt.Sprintf("%s%s%d", x, kind, i) }
	const accounts, teamCount, machines = 5, 3, 10
	for i := range accounts {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, id("u", i))
	}
	for i := range teamCount {
		exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, id("t", i), id("u", i))
		exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$1,'active','team-pro',1,now()-interval '1 hour',now()+interval '1 hour')`, id("t", i))
		for a := range accounts {
			exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, id("t", i), id("u", a))
		}
	}
	bind := func(kind, resource string) {
		for i := range teamCount {
			exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,$2,$3,'')`, id("t", i), kind, resource)
			for a := range accounts {
				if kind != "machine" {
					exec(`INSERT INTO paperboat.team_resource_grants(team_id,account_id,resource_kind,resource_id,permission,generation,active) VALUES($1,$2,$3,$4,'use',1,true)`, id("t", i), id("u", a), kind, resource)
				}
			}
		}
	}
	for i := range machines {
		m, owner := id("m", i), id("u", i%accounts)
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$1,$3,'linux','amd64','/w','online','occupied',true,1)`, m, owner, fmt.Sprintf("m%d", i))
		exec(`INSERT INTO paperboat.machine_service_snapshots(machine_id,installation_generation,boot_id,started_at,generation,services,expires_at,updated_at)
 VALUES($1,1,'boot-012345678901',now(),1,'[{"port":22,"address_family":"ipv4"},{"port":80,"address_family":"ipv4"},{"port":443,"address_family":"ipv4"},{"port":1023,"address_family":"ipv4"},{"port":1024,"address_family":"ipv4"},{"port":5432,"address_family":"ipv4"},{"port":8080,"address_family":"ipv6"}]',now()+interval '1 hour',now())`, m)
		exec(`INSERT INTO paperboat.lazy_access_policies(id,hostname,account_id,environment_id,machine_id,installation_generation,generation,target_scheme,target_address,access_mode,ownership_mode,expires_at) VALUES($1,$1||'.example.test',$2,$1,$3,1,1,'http','127.0.0.1:22','private','persistent_port',now()+interval '1 hour')`, id("l", i), owner, m)
		exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,'active','private',1,'endpoint_'||overlay(overlay(gen_random_uuid()::text placing '4' from 15) placing '8' from 20),'https://'||$1||'.example.test',$3,$2)`, id("n", i), owner, m)
		exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,priority,protocol,match_type,origin_scheme,origin_address,tls_verification,generation,desired_state,created_by_actor_id,updated_by_actor_id) VALUES($1,$1,'db',2,'private_tcp','catch_all','tcp','[::1]:22','not_applicable',1,'active',$2,$2)`, id("n", i), owner)
		bind("machine", m)
		bind("lazy_policy", id("l", i))
		bind("tunnel", id("n", i))
	}
	like := x + "%"
	granted, denied, teamGranted := 0, 0, 0
	for round := range 40 {
		for _, q := range []string{
			`UPDATE paperboat.users SET status=CASE WHEN random()<.06 THEN 'suspended' ELSE 'active' END WHERE id LIKE $1`,
			`UPDATE paperboat.teams t SET suspended=r.s,suspended_at=CASE WHEN r.s THEN now() END,suspended_by=CASE WHEN r.s THEN 'test' END,deleted_at=CASE WHEN random()<.06 THEN now() END,sso_required=random()<.3,sso_session_seconds=3600 FROM (SELECT team_id,random()<.08 AS s FROM paperboat.teams) r WHERE r.team_id=t.team_id AND t.team_id LIKE $1`,
			`UPDATE paperboat.billing_workspaces SET state=(ARRAY['active','active','active','trialing','past_due','canceled'])[1+floor(random()*6)::int],period_end=now()+CASE WHEN random()<.1 THEN -1 ELSE 1 END*interval '10 minutes',renewal_grace_until=now()+(random()-.5)*interval '1 hour' WHERE team_id LIKE $1`,
			`UPDATE paperboat.team_members SET active=random()<.85,role=CASE WHEN random()<.3 THEN 'admin' ELSE 'member' END,sso_exempt=random()<.2,sso_verified_at=CASE WHEN random()<.7 THEN now()-random()*interval '2 hours' END WHERE team_id LIKE $1`,
			`DELETE FROM paperboat.workspace_policy_rules WHERE policy_id IN (SELECT id FROM paperboat.workspace_policies WHERE team_id LIKE $1)`,
			`DELETE FROM paperboat.workspace_policy_tags WHERE policy_id IN (SELECT id FROM paperboat.workspace_policies WHERE team_id LIKE $1)`,
			`DELETE FROM paperboat.workspace_policy_groups WHERE policy_id IN (SELECT id FROM paperboat.workspace_policies WHERE team_id LIKE $1)`,
			`DELETE FROM paperboat.workspace_policies WHERE team_id LIKE $1`,
			`INSERT INTO paperboat.workspace_policies(team_id,version,source,digest,allow_private_access) SELECT team_id,1,'{}','test',random()<.8 FROM paperboat.teams WHERE team_id LIKE $1 AND random()<.9`,
			`UPDATE paperboat.user_machines SET owner_team_id=CASE WHEN random()<.5 THEN $2||'t'||floor(random()*3)::int END,personal_auto_enabled=random()<.5,seat_state=CASE WHEN random()<.08 THEN 'released' ELSE 'occupied' END,revoked_at=CASE WHEN random()<.06 THEN now() END WHERE id LIKE $1`,
			`UPDATE paperboat.team_resource_bindings b SET active=random()<.6,owner_account=CASE WHEN random()<.15 THEN $2||'u0' ELSE
 coalesce((SELECT user_id FROM paperboat.user_machines m WHERE m.id=b.resource_id),(SELECT account_id FROM paperboat.tunnels n WHERE n.id=b.resource_id),(SELECT account_id FROM paperboat.lazy_access_policies l WHERE l.id=b.resource_id)) END WHERE team_id LIKE $1`,
			`UPDATE paperboat.team_resource_grants SET active=random()<.5,permission=CASE WHEN resource_kind='tunnel' AND random()<.3 THEN 'inspect' ELSE (ARRAY['use','manage'])[1+floor(random()*2)::int] END WHERE team_id LIKE $1`,
			`UPDATE paperboat.lazy_access_policies SET access_mode=CASE WHEN random()<.5 THEN 'team' ELSE 'private' END,authorization_source=CASE WHEN random()<.2 THEN 'machine_service' ELSE 'explicit' END,target_address=(ARRAY['127.0.0.1:22','[::1]:80','127.0.0.1:9'])[1+floor(random()*3)::int],installation_generation=CASE WHEN random()<.1 THEN 2 ELSE 1 END,expires_at=now()+(random()-.2)*interval '1 hour',deleted_at=CASE WHEN random()<.1 THEN now() END WHERE id LIKE $1`,
			`UPDATE paperboat.tunnels SET access_mode=(ARRAY['public','private','team'])[1+floor(random()*3)::int],owner_team_id=CASE WHEN random()<.4 THEN $2||'t'||floor(random()*3)::int END,account_id=CASE WHEN random()<.1 THEN $2||'u0' ELSE (SELECT user_id FROM paperboat.user_machines m WHERE m.id=created_by_host_id) END,(desired_state,deleted_at)=(SELECT s,CASE WHEN s='deleted' THEN now() END FROM (SELECT (ARRAY['paused','deleted','active','active','active','active','active','active'])[1+floor(random()*8)::int] AS s WHERE tunnels.id IS NOT NULL) d),created_at=now()-interval '2 hours',expires_at=CASE WHEN random()<.4 THEN now()+(random()-.2)*interval '1 hour' END WHERE id LIKE $1`,
			`UPDATE paperboat.tunnel_routes SET protocol=CASE WHEN random()<.1 THEN 'http' ELSE 'private_tcp' END,(desired_state,deleted_at)=(SELECT s,CASE WHEN s='deleted' THEN now() END FROM (SELECT (ARRAY['disabled','deleted','active','active','active','active','active','active'])[1+floor(random()*8)::int] AS s WHERE tunnel_routes.id IS NOT NULL) d),origin_address=(ARRAY['127.0.0.1:22','[::1]:22','[::1]:5432'])[1+floor(random()*3)::int] WHERE tunnel_id LIKE $1`,
			`INSERT INTO paperboat.workspace_policy_groups(policy_id,group_name,account_id) SELECT p.id,'g',u.id FROM paperboat.workspace_policies p,paperboat.users u WHERE p.team_id LIKE $1 AND u.id LIKE $1 AND random()<.5`,
			`INSERT INTO paperboat.workspace_policy_tags(policy_id,tag,machine_id) SELECT p.id,tag,m.id FROM paperboat.workspace_policies p,paperboat.user_machines m,unnest(ARRAY['a','b']) tag WHERE p.team_id LIKE $1 AND m.id LIKE $1 AND random()<.4`,
			`INSERT INTO paperboat.workspace_policy_rules(policy_id,rule,deny,subject_kind,subject_id,target_kind,target_id,action,ports)
 SELECT r.policy_id,r.n,r.deny,r.sk,CASE r.sk WHEN 'all' THEN '' WHEN 'account' THEN $2||'u'||r.pick%5 WHEN 'group' THEN 'g' ELSE (ARRAY['owner','admin','member'])[1+r.pick%3] END,
 r.tk,CASE r.tk WHEN 'all_machines' THEN '' WHEN 'machine' THEN $2||'m'||r.pick%10 ELSE (ARRAY['a','b'])[1+r.pick%2] END,r.action,r.ports
 FROM (SELECT p.id policy_id,n,random()<.25 deny,(ARRAY['all','role','account','group'])[1+floor(random()*4)::int] sk,(ARRAY['all_machines','machine','tag'])[1+floor(random()*3)::int] tk,
  floor(random()*30)::int pick,CASE WHEN random()<.85 THEN 'private_access' ELSE 'terminal' END action,(ARRAY[int4range(1,65536),int4range(22,23),int4range(80,6000),int4range(5000,9000),int4range(1,1024),int4range(1024,65536)])[1+floor(random()*6)::int] ports
  FROM paperboat.workspace_policies p,generate_series(0,5) n WHERE p.team_id LIKE $1) r`,
		} {
			if strings.Contains(q, "$2") {
				exec(q, like, x)
			} else {
				exec(q, like)
			}
		}
		for a := range accounts {
			actor := id("u", a)
			items, e := machineservices.New(store, "local.example.test").List(ctx, actor)
			if e != nil {
				t.Fatal(e)
			}
			shown := map[string]bool{}
			for _, item := range items {
				for _, p := range item.Services {
					shown[fmt.Sprint(item.MachineID, " ", p.Port)] = true
				}
			}
			tx, e := store.SQL().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if e != nil {
				t.Fatal(e)
			}
			for i := range machines {
				for _, port := range []int{22, 80, 443, 1023, 1024, 5432, 8080} {
					authority, e := machineservices.ResolveTx(ctx, tx, actor, id("m", i), port)
					if e != nil && e != machineservices.ErrDenied {
						t.Fatal(e)
					}
					if key := fmt.Sprint(id("m", i), " ", port); shown[key] != (e == nil) {
						t.Fatalf("round %d: %s on %s: list %v, ResolveTx %+v %v", round, actor, key, shown[key], authority, e)
					}
					if e != nil {
						denied++
					} else if granted++; authority.TeamID != "" {
						teamGranted++
					}
				}
			}
			if e = tx.Rollback(); e != nil {
				t.Fatal(e)
			}
		}
	}
	t.Logf("granted %d (%d through a team), denied %d", granted, teamGranted, denied)
	if granted < 100 || teamGranted < 100 || denied < 100 {
		t.Fatal("the fixtures no longer reach both answers")
	}
}
