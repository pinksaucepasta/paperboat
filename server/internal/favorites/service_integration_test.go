package favorites

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/teams/teamstest"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

func TestWorkspaceScopedFavoritesListAndMutation(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run favorite integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	suffix := fmt.Sprint(time.Now().UnixNano())
	actor, hostOwner := "fav_actor_"+suffix, "fav_host_owner_"+suffix
	teamOne, teamTwo := "fav-one-"+suffix, "fav-two-"+suffix
	sharedMachine, personalMachine := "fav_shared_"+suffix, "fav_personal_"+suffix
	for _, user := range []string{actor, hostOwner} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status)
VALUES($1,$2,$3,'active')`, user, "workos_"+user, user+"@invalid.test"); err != nil {
			t.Fatal(err)
		}
	}
	for _, team := range []string{teamOne, teamTwo} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, hostOwner); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active)
VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)`, team, hostOwner, actor); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end)
VALUES($1,$2,'active','team-pro',2,$3,$4)`, "bw_"+team, team, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	for _, fixture := range []struct{ id, owner string }{{sharedMachine, hostOwner}, {personalMachine, actor}} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines
(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities)
VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,ARRAY['terminal_host'],ARRAY['terminal_host'])`,
			fixture.id, fixture.owner, "env_"+fixture.id); err != nil {
			t.Fatal(err)
		}
	}
	for _, team := range []string{teamOne, teamTwo} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation)
VALUES($1,'machine',$2,$3,true,1)`, team, sharedMachine, hostOwner); err != nil {
			t.Fatal(err)
		}
	}
	teamstest.SetMachineAccess(t, store, teamOne, sharedMachine, actor, "terminal")
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`DELETE FROM paperboat.user_favorites WHERE user_id=$1`, []any{actor}},
			{`DELETE FROM paperboat.team_resource_bindings WHERE resource_kind='machine' AND resource_id=$1`, []any{sharedMachine}},
			{`DELETE FROM paperboat.billing_workspaces WHERE team_id=ANY($1::text[])`, []any{[]string{teamOne, teamTwo}}},
			{`DELETE FROM paperboat.team_members WHERE team_id=ANY($1::text[])`, []any{[]string{teamOne, teamTwo}}},
			{`DELETE FROM paperboat.teams WHERE team_id=ANY($1::text[])`, []any{[]string{teamOne, teamTwo}}},
			{`DELETE FROM paperboat.user_machine_terminal_sessions WHERE user_machine_id=$1`, []any{personalMachine}},
			{`DELETE FROM paperboat.user_machines WHERE id=ANY($1::text[])`, []any{[]string{sharedMachine, personalMachine}}},
			{`DELETE FROM paperboat.users WHERE id=ANY($1::text[])`, []any{[]string{actor, hostOwner}}},
		} {
			if _, err := store.SQL().ExecContext(cleanupCtx, statement.query, statement.args...); err != nil {
				t.Errorf("favorite fixture cleanup: %v", err)
				return
			}
		}
	})

	service := NewService(store)
	personal := workspace.WithContext(ctx, workspace.Scope{ID: "personal"})
	selectedOne := workspace.WithContext(ctx, workspace.Scope{ID: teamOne, TeamID: teamOne})
	selectedTwo := workspace.WithContext(ctx, workspace.Scope{ID: teamTwo, TeamID: teamTwo})
	if err := service.Set(personal, actor, "machine", personalMachine, true); err != nil {
		t.Fatalf("favorite Personal machine: %v", err)
	}
	if err := service.Set(selectedOne, actor, "machine", sharedMachine, true); err != nil {
		t.Fatalf("favorite exact team-granted machine: %v", err)
	}
	if err := service.Set(selectedTwo, actor, "machine", sharedMachine, true); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("other team reused Team One machine grant: %v", err)
	}
	oneItems, err := service.List(selectedOne, actor)
	if err != nil || len(oneItems) != 1 || oneItems[0].ResourceID != sharedMachine {
		t.Fatalf("Team One favorites = %#v, %v", oneItems, err)
	}
	twoItems, err := service.List(selectedTwo, actor)
	if err != nil || len(twoItems) != 0 {
		t.Fatalf("Team Two favorites = %#v, %v; want no Team One resource", twoItems, err)
	}
	personalItems, err := service.List(personal, actor)
	if err != nil || len(personalItems) != 1 || personalItems[0].ResourceID != personalMachine {
		t.Fatalf("Personal favorites = %#v, %v", personalItems, err)
	}
	// Unscoped favorites must hide removed machines and their terminal sessions.
	session := "fav_session_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_terminal_sessions (id,user_machine_id,owner_account,name,launch_cwd) VALUES ($1,$2,$3,'test','')`, session, personalMachine, actor); err != nil {
		t.Fatal(err)
	}
	if err := service.Set(ctx, actor, "session", personalMachine+":"+session, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET deleted_at=now(),state='deleted' WHERE id=$1`, personalMachine); err != nil {
		t.Fatal(err)
	}
	items, err := service.List(ctx, actor)
	if err != nil || len(items) != 1 || items[0].ResourceID != sharedMachine {
		t.Fatalf("favorites after removal = %#v, %v", items, err)
	}
	if err := service.Set(ctx, actor, "machine", personalMachine, true); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("favorited removed machine: %v", err)
	}
	if err := service.Set(selectedTwo, actor, "machine", sharedMachine, false); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("other team removed a favorite outside its scope: %v", err)
	}
	if err := service.Set(selectedOne, actor, "machine", sharedMachine, false); err != nil {
		t.Fatalf("remove favorite in selected Team One: %v", err)
	}
}
