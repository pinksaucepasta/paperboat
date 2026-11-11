package usermachines

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
)

// The reset cutover replaces a record only inside one ownership scope, names the loser of a race, and takes
// the team lock the alias trigger takes.
func TestReplaceMachineScopeAndRaces(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	user := "usr_scope_" + suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1,$2,$3,'active')`, user, "workos_"+user, user+"@example.test")
	teamA, teamB := "team_a_"+suffix, "team_b_"+suffix
	for _, team := range []string{teamA, teamB} {
		exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, user)
		exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, team, user)
	}
	add := func(id, alias, team string) {
		t.Helper()
		var owner any
		if team != "" {
			owner = team
		}
		exec(`INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation,owner_team_id) VALUES ($1,$2,$3,$4,'linux','amd64','/workspace','online','occupied',true,1,$5)`, id, user, "env_"+id, alias, owner)
	}
	state := func(id string) (alias string, deleted bool) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, `SELECT alias,deleted_at IS NOT NULL FROM paperboat.user_machines WHERE id=$1`, id).Scan(&alias, &deleted); err != nil {
			t.Fatal(err)
		}
		return
	}
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	id := func(name string) string { return "mch_" + name + "_" + suffix }

	// Across scopes nothing changes: personal<->team and teamA<->teamB.
	add(id("pp"), "desk", "")
	add(id("pt"), "desk-t", teamA)
	add(id("tp"), "rack", teamA)
	add(id("tp2"), "rack-host", "")
	add(id("ta"), "lab", teamA)
	add(id("tb"), "lab-b", teamB)
	for _, pair := range [][2]string{{"pp", "pt"}, {"tp", "tp2"}, {"ta", "tb"}} {
		if err := service.ReplaceMachine(ctx, user, id(pair[0]), id(pair[1])); !errors.Is(err, ErrReplacementTeamMismatch) {
			t.Fatalf("replace %v across scopes: %v, want ErrReplacementTeamMismatch", pair, err)
		}
		if _, deleted := state(id(pair[0])); deleted {
			t.Fatalf("%s was removed by a refused cross-scope replacement", pair[0])
		}
	}
	if alias, _ := state(id("tb")); alias != "lab-b" {
		t.Fatalf("a different team's machine took the name: %q", alias)
	}

	// Inside one team the replacement takes the name (under the team lock).
	add(id("t1"), "ci-box", teamB)
	add(id("t2"), "builder-7", teamB)
	if err := service.ReplaceMachine(ctx, user, id("t1"), id("t2")); err != nil {
		t.Fatalf("replace inside one team: %v", err)
	}
	if alias, deleted := state(id("t2")); alias != "ci-box" || deleted {
		t.Fatalf("team replacement: alias=%q deleted=%v, want ci-box", alias, deleted)
	}

	// Two new machines race for one old record: one wins, the other is told, a retry by the winner is a no-op.
	add(id("old"), "studio", "")
	add(id("newa"), "host-a", "")
	add(id("newb"), "host-b", "")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, replacement := range []string{id("newa"), id("newb")} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = service.ReplaceMachine(ctx, user, id("old"), replacement)
		}()
	}
	wg.Wait()
	winner, loser := id("newa"), id("newb")
	if errs[0] != nil {
		winner, loser = loser, winner
		errs[0], errs[1] = errs[1], errs[0]
	}
	if errs[0] != nil || !errors.Is(errs[1], ErrAlreadyReplaced) {
		t.Fatalf("race: winner err=%v, loser err=%v, want nil and ErrAlreadyReplaced", errs[0], errs[1])
	}
	if alias, _ := state(winner); alias != "studio" {
		t.Fatalf("winner alias=%q, want studio", alias)
	}
	if alias, _ := state(loser); alias == "studio" {
		t.Fatal("both machines took the name")
	}
	if err := service.ReplaceMachine(ctx, user, id("old"), winner); err != nil {
		t.Fatalf("a retry by the machine that replaced it must stay a no-op: %v", err)
	}
	if err := service.ReplaceMachine(ctx, user, id("old"), loser); !errors.Is(err, ErrAlreadyReplaced) {
		t.Fatalf("a later attempt by the loser: %v, want ErrAlreadyReplaced", err)
	}
}
