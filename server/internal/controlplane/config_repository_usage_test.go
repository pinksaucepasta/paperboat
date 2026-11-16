package controlplane

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// A custom Git repository can be a team default, and the status and repository list say who chose each
// machine's repository (team_default > tag_rule > recipe > machine) and how many machines use it.
func TestConfigRepositoryUsageSourceAndGitTeamDefault(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(t.Name(), "/", "_")
	owner, member := "cfgsrc_owner_"+suffix, "cfgsrc_member_"+suffix
	teamID := fmt.Sprintf("cfgsrc-%x", sha256.Sum256([]byte(t.Name())))[:17] // a workspace id is a lowercase slug
	environmentID, machineID, recipeID := "cfgsrc_env_"+suffix, "cfgsrc_machine_"+suffix, "cfgsrc_recipe_"+suffix
	cleanupConfigTeamLifecycleFixture(t, store, []string{owner, member}, []string{teamID}, []string{environmentID}, []string{machineID})
	t.Cleanup(func() { // runs before the fixture cleanup: recipes reference the users
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := store.SQL().ExecContext(ctx, `DELETE FROM paperboat.recipes WHERE id=$1`, recipeID); err != nil {
			t.Error(err)
		}
	})
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []string{owner, member} {
		exec(`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, user, "workos_"+user, user+"@example.test")
	}
	exec(`INSERT INTO paperboat.teams (team_id,owner_account,generation) VALUES ($1,$2,1)`, teamID, owner)
	// Selecting a team workspace needs an active membership row, also for the team's owner.
	exec(`INSERT INTO paperboat.team_members (team_id,account_id,membership_generation,role,active) VALUES ($1,$2,1,'admin',true),($1,$3,1,'member',true)`, teamID, owner, member)
	exec(`INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id) VALUES ($1,$2,$3)`, environmentID, "workspace_"+suffix, member)
	exec(`INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root) VALUES ($1,$2,$3,'machine','linux','arm64','/workspace')`, machineID, member, environmentID)

	service := NewConfigAssignmentService(store, nil, "warning")
	statuses := NewConfigStatusService(store, nil, 10)
	teamContext := workspace.WithContext(ctx, workspace.Scope{ID: teamID, TeamID: teamID})
	const url = "https://git.example.test/team/config.git"
	ownerRepo, err := service.ConnectRepository(ctx, owner, "git", url, "config")
	if err != nil {
		t.Fatal(err)
	}
	memberRepo, err := service.ConnectRepository(ctx, member, "git", url, "config")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetTeamDefault(teamContext, owner, teamID, ownerRepo.ID, 0); err != nil {
		t.Fatalf("a custom Git repository was refused as the team default: %v", err)
	}
	listed := func(ctx context.Context, user string) ConfigRepositoryItem {
		t.Helper()
		items, _, err := service.RepositoryInventory(ctx, user, "", "", "", 100, 0)
		if err != nil || len(items) != 1 {
			t.Fatalf("repositories=%+v err=%v", items, err)
		}
		return items[0]
	}
	if item := listed(teamContext, owner); !item.TeamDefault || item.MachineCount != 0 {
		t.Fatalf("team workspace list: %+v", item)
	}
	if listed(ctx, owner).TeamDefault {
		t.Fatal("the personal workspace marked a repository as a team default")
	}
	if adoption, err := service.AdoptTeamDefault(ctx, member, teamID, 1); err != nil || adoption.RepositoryID != memberRepo.ID {
		t.Fatalf("adoption of the Git team default=%+v err=%v", adoption, err)
	}

	source := func() string {
		t.Helper()
		full, err := statuses.Account(ctx, member)
		if err != nil || len(full.Environments) != 1 {
			t.Fatalf("status=%+v err=%v", full, err)
		}
		page, err := statuses.AccountInventory(ctx, member, ConfigStatusInventoryOptions{Limit: 50})
		if err != nil || len(page.Environments) != 1 || page.Environments[0].Source != full.Environments[0].Source {
			t.Fatalf("inventory=%+v err=%v (full source %q)", page.Environments, err, full.Environments[0].Source)
		}
		return full.Environments[0].Source
	}
	if got := source(); got != "team_default" {
		t.Fatalf("adopted source=%q", got)
	}
	if item := listed(ctx, member); item.MachineCount != 1 {
		t.Fatalf("member repository machine count=%d", item.MachineCount)
	}
	if err := service.UnadoptTeamDefault(ctx, member); err != nil {
		t.Fatal(err)
	}
	// Unadopting revokes the machine's assignment: it is Off (not "needs consent") and back in the personal
	// workspace, on both status paths (11-17: 18 unadopted machines still showed consent_required in the team).
	for _, scope := range []struct {
		ctx  context.Context
		rows int
	}{{teamContext, 0}, {workspace.WithContext(ctx, workspace.Scope{ID: "personal"}), 1}} {
		full, err := statuses.Account(scope.ctx, member)
		if err != nil || len(full.Environments) != scope.rows {
			t.Fatalf("unadopted full status=%+v err=%v want %d rows", full.Environments, err, scope.rows)
		}
		page, err := statuses.AccountInventory(scope.ctx, member, ConfigStatusInventoryOptions{Limit: 50})
		if err != nil || len(page.Environments) != scope.rows {
			t.Fatalf("unadopted inventory=%+v err=%v want %d rows", page.Environments, err, scope.rows)
		}
		if scope.rows == 1 && (full.Environments[0].State != "disabled" || page.Environments[0].State != "disabled") {
			t.Fatalf("unadopted machine state full=%q inventory=%q, want disabled", full.Environments[0].State, page.Environments[0].State)
		}
	}
	current, err := service.Assignment(ctx, member, machineID)
	if err != nil {
		t.Fatal(err)
	}
	current, err = assignConfigTestProjection(service, ctx, member, machineID, memberRepo.ID, ConfigModePullOnly, "warning", current.Version)
	if err != nil {
		t.Fatal(err)
	}
	if got := source(); got != "machine" {
		t.Fatalf("machine-chosen source=%q", got)
	}
	auth := sha256.Sum256([]byte(recipeID))
	exec(`INSERT INTO paperboat.recipes (id,token_id,account_id,name,source,source_format,canonical,auth_hash,expires_at)
VALUES ($1,$1,$2,'cfgsrc','','json','{"config":{"repository":"config"}}',$3,now()+interval '1 day')`, recipeID, member, auth[:])
	exec(`INSERT INTO paperboat.recipe_machines (machine_id,recipe_id) VALUES ($1,$2)`, machineID, recipeID)
	if got := source(); got != "recipe" {
		t.Fatalf("recipe-chosen source=%q", got)
	}
	current, err = service.ConfigureProjection(ctx, member, machineID, memberRepo.ID, "", ConfigModePullOnly, false, "warning", current.Version, nil, nil, testConfigurationRevision, ConfigSourceTagRule)
	if err != nil {
		t.Fatal(err)
	}
	if got := source(); got != "tag_rule" {
		t.Fatalf("rule-chosen source=%q", got)
	}
	if err := service.Clear(ctx, member, machineID, current.Version); err != nil {
		t.Fatal(err)
	}
	if got := source(); got != "" {
		t.Fatalf("unassigned source=%q", got)
	}
	if item := listed(ctx, member); item.MachineCount != 0 {
		t.Fatalf("cleared repository machine count=%d", item.MachineCount)
	}
}
