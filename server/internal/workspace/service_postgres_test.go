package workspace

import (
	"context"
	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceNamesUseCurrentTeamNamePostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	owner, other := "ws_owner_"+suffix, "ws_other_"+suffix
	named, fallback, foreign := "ws-named-"+suffix, "ws-fallback-"+suffix, "ws-foreign-"+suffix
	execute := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, q := range []string{`DELETE FROM teams WHERE team_id IN($1,$2,$3) AND $4<>'' AND $5<>''`, `DELETE FROM users WHERE id IN($4,$5) AND $1<>'' AND $2<>'' AND $3<>''`} {
			if _, err := store.SQL().ExecContext(clean, q, named, fallback, foreign, owner, other); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	}()
	execute(`INSERT INTO users(id,workos_subject,primary_email,status) SELECT x,x,x||'@workspace.test','active' FROM unnest($1::text[]) x`, []string{owner, other})
	execute(`INSERT INTO teams(team_id,owner_account,generation,display_name) VALUES($1,$4,1,'API audit dev'),($2,$4,1,NULL),($3,$5,1,'Private team')`, named, fallback, foreign, owner, other)
	execute(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$4,1,'admin',true),($2,$4,1,'admin',true),($3,$5,1,'admin',true)`, named, fallback, foreign, owner, other)
	service := New(store)
	check := func(expected string) {
		t.Helper()
		items, err := service.List(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]Item{}
		for _, item := range items {
			seen[item.ID] = item
		}
		if len(items) != 3 || seen[named].Name != expected || seen[named].Role != "owner" || seen[fallback].Name != fallback {
			t.Fatalf("workspace names/authority: %+v", items)
		}
	}
	check("API audit dev")
	execute(`UPDATE teams SET display_name='Renamed development team',generation=generation+1 WHERE team_id=$1`, named)
	check("Renamed development team")
	items, err := service.List(ctx, other)
	if err != nil || len(items) != 2 || items[1].ID != foreign || items[1].Name != "Private team" {
		t.Fatalf("foreign account inventory: %+v %v", items, err)
	}
	execute(`UPDATE team_members SET active=false WHERE team_id=$1 AND account_id=$2`, named, owner)
	items, err = service.List(ctx, owner)
	if err != nil || len(items) != 2 {
		t.Fatalf("removed membership inventory: %+v %v", items, err)
	}
}

// The list says which teams need an SSO sign-in the viewer lacks: the console labels them (needsSso) and the owner, an
// SSO-exempt member and a member with a recent sign-in are not asked.
func TestWorkspaceListSaysWhenATeamNeedsSSOPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	owner, member, exempt, fresh := "sso_owner_"+suffix, "sso_member_"+suffix, "sso_exempt_"+suffix, "sso_fresh_"+suffix
	needs, plain := "sso-needs-"+suffix, "sso-plain-"+suffix
	execute := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, q := range []string{`DELETE FROM teams WHERE team_id IN($1,$2) AND $3<>''`, `DELETE FROM users WHERE id=ANY($1::text[])`} {
			args := []any{needs, plain, owner}
			if strings.Contains(q, "users") {
				args = []any{[]string{owner, member, exempt, fresh}}
			}
			if _, err := store.SQL().ExecContext(clean, q, args...); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	}()
	execute(`INSERT INTO users(id,workos_subject,primary_email,status) SELECT x,x,x||'@workspace.test','active' FROM unnest($1::text[]) x`, []string{owner, member, exempt, fresh})
	execute(`INSERT INTO teams(team_id,owner_account,generation,display_name,sso_required) VALUES($1,$3,1,'Needs SSO',true),($2,$3,1,'Plain',false)`, needs, plain, owner)
	execute(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active,sso_exempt,sso_verified_at) VALUES
		($1,$2,1,'admin',true,false,NULL),($1,$3,1,'member',true,false,NULL),($1,$4,1,'member',true,true,NULL),($1,$5,1,'member',true,false,now()),($6,$3,1,'member',true,false,NULL)`,
		needs, owner, member, exempt, fresh, plain)
	execute(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, plain, owner)
	service := New(store)
	state := func(account, team string) (bool, *bool) {
		t.Helper()
		items, err := service.List(ctx, account)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if item.ID == team {
				return item.SSORequired, item.SSOSignedIn
			}
		}
		t.Fatalf("%s does not list %s: %+v", account, team, items)
		return false, nil
	}
	for account, want := range map[string]bool{owner: true, member: false, exempt: true, fresh: true} {
		required, signedIn := state(account, needs)
		if !required || signedIn == nil || *signedIn != want {
			t.Errorf("%s on the SSO team: required=%v signedIn=%v, want signed in %v", account, required, signedIn, want)
		}
	}
	if required, signedIn := state(member, plain); required || signedIn != nil {
		t.Errorf("a team without SSO says required=%v signedIn=%v", required, signedIn)
	}
}
