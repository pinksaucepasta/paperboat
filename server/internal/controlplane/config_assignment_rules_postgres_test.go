package controlplane

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
)

// A machine's config repository rule comes from its team policies first, then the owner's
// personal policy; the first rule matching machine:* or one of its tags wins, and the URL
// resolves to the owner's connected repository with that branch.
func TestConfigMachineRuleTeamBeforePersonalAndOwnerRepository(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(t.Name(), "/", "_")
	user, env, machine, team := "cfgrule_user_"+suffix, "cfgrule_env_"+suffix, "cfgrule_machine_"+suffix, "cfgrule-team-"+strings.ToLower(suffix)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, user, "workos_"+user, user+"@example.test")
	exec(`INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id) VALUES ($1,$2,$3)`, env, "workspace_"+suffix, user)
	key := enrollmentTestPrivateKey(env).Public().(ed25519.PublicKey)
	exec(`INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,public_identity_key,seat_state) VALUES ($1,$2,$3,'machine','linux','arm64','/workspace',$4,'occupied')`, machine, user, env, base64.RawURLEncoding.EncodeToString(key))
	service := NewConfigAssignmentService(store, nil, "warning-1")

	rule, err := service.MachineRule(ctx, user, machine)
	if err != nil || rule.Matched || rule.State != "none" {
		t.Fatalf("no policy: %+v %v", rule, err)
	}
	if _, err := service.MachineRule(ctx, "someone-else", machine); err != ErrAssignmentForbidden {
		t.Fatalf("another account read the rule: %v", err)
	}

	policy := func(column, key, source string) int64 {
		t.Helper()
		var id int64
		if err := store.SQL().QueryRowContext(ctx, `INSERT INTO paperboat.workspace_policies(`+column+`,version,source,digest) VALUES($1,1,$2,'sha256:x') RETURNING id`, key, source).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	policy("owner_account", user, `{"config_sync":[{"dst":["machine:*"],"repository":"https://git.example.test/me/dotfiles.git"}]}`)
	rule, err = service.MachineRule(ctx, user, machine)
	if err != nil || !rule.Matched || rule.TeamID != "" || rule.Target != "machine:*" || rule.Branch != "main" || rule.State != "connect_on_apply" || rule.RepositoryID != "" {
		t.Fatalf("personal rule, repository not connected: %+v %v", rule, err)
	}

	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, user)
	teamPolicy := policy("team_id", team, `{"tags":{"tag:prod":["machine:#`+machine+`"]},"config_sync":[
		{"dst":["tag:ci"],"repository":"https://git.example.test/acme/ci.git"},
		{"dst":["tag:prod"],"repository":"https://git.example.test/acme/prod.git","branch":"live"}]}`)
	exec(`INSERT INTO paperboat.workspace_policy_tags(policy_id,tag,machine_id) VALUES($1,'tag:prod',$2)`, teamPolicy, machine)
	rule, _ = service.MachineRule(ctx, user, machine)
	if rule.TeamID != "" {
		t.Fatalf("a team the machine is not shared into decided: %+v", rule)
	}
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'machine',$2,$3)`, team, machine, user)
	connected, err := service.ConnectRepository(ctx, user, "git", "https://git.example.test/acme/prod.git", "Prod", "live")
	if err != nil {
		t.Fatal(err)
	}
	rule, err = service.MachineRule(ctx, user, machine)
	if err != nil || rule.TeamID != team || rule.Target != "tag:prod" || rule.Repository != "https://git.example.test/acme/prod.git" || rule.Branch != "live" || rule.State != "connected" || rule.RepositoryID != connected.ID {
		t.Fatalf("team tag rule: %+v %v", rule, err)
	}
}
