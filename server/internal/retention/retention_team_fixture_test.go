package retention

import (
	"strings"
	"testing"
)

// insertDeletedTeamRows inserts at least one valid row for every table in
// deletedTeamTables, plus one row for each cascading child table named in
// retention.go: team_resource_grants, team_terminal_session_grants and
// team_machine_grants (from team_members/team_resource_bindings),
// team_config_default_adoptions (from team_config_defaults), and the
// workspace_policy_* children of workspace_policies.
//
// team must be a fresh teams row (deleted_at already set by the caller) and
// user must be an existing users row. Prerequisite rows (an aux user, a
// machine, a terminal session, a tunnel plus route, and a config repository)
// use ids derived from team so two teams never collide.
func insertDeletedTeamRows(t *testing.T, exec func(string, ...any), team, user string) {
	t.Helper()
	safe := strings.ReplaceAll(strings.ToLower(team), "_", "-")
	aux := team + "-aux"
	machine := "mch-" + safe
	env := "env-" + safe
	session := "ses-" + safe
	tunnel := "tun-" + safe
	route := "rte-" + safe
	repo := "repo-" + safe
	envRes := safe + "-env"
	alias := "a-" + safe
	if len(alias) > 63 {
		alias = strings.TrimRight(alias[:63], "-")
	}

	// Prerequisite user for the adoption row: team_config_default_adoptions
	// keys on account_id, so two teams cannot share the adopter.
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, aux)
	// Prerequisite machine, terminal session, tunnel route and config repo.
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,$4,'linux','amd64','/workspace','offline','released')`, machine, user, env, alias)
	exec(`INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,owner_account,name,launch_cwd) VALUES($1,$2,$3,'shared','/workspace')`, session, machine, user)
	exec(`WITH e AS (SELECT gen_random_uuid() AS id) INSERT INTO paperboat.tunnels(id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) SELECT $1,$2,$4,'endpoint_'||e.id::text,'https://'||replace(e.id::text,'-','')||'.example.test',$3,$2 FROM e`, tunnel, user, machine, "tfx-"+safe)
	exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id) VALUES($1,$2,'web','http','catch_all','http','127.0.0.1:3000',$3,$3)`, route, tunnel, user)
	exec(`INSERT INTO paperboat.control_config_repositories(id,owner_user_id,provider,external_ref,display_name) VALUES($1,$2,'github',$3,'repo display')`, repo, user, "ext-"+safe)

	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, user)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active) VALUES($1,'env',$2,$3,true)`, team, envRes, user)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active) VALUES($1,'machine',$2,$3,true)`, team, machine, user)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active) VALUES($1,'terminal_session',$2,$3,false)`, team, session, user)
	exec(`INSERT INTO paperboat.team_invitations(invitation_id,team_id,recipient_account,created_by,expires_at,cancelled_at) VALUES('inv_'||$1,$1,$2,$2,now(),now())`, team, user)
	exec(`INSERT INTO paperboat.team_config_defaults(team_id,provider,external_repository_id,display_name,branch,updated_by) VALUES($1,'github',$2,'cfg display','main',$3)`, team, "extrepo-"+safe, user)
	exec(`INSERT INTO paperboat.team_receipt_smtp(team_id,host,port,username,password_ciphertext,from_address,tls_mode) VALUES($1,'smtp.example.test',587,'smtp-user',decode('00','hex'),'team@example.test','starttls')`, team)
	exec(`INSERT INTO paperboat.team_machine_requests(team_id,machine_id,requested_by,state) VALUES($1,$2,$3,'pending')`, team, machine, user)
	exec(`INSERT INTO paperboat.team_jit_requests(team_id,account_id) VALUES($1,$2)`, team, user)
	exec(`INSERT INTO paperboat.team_sso_domains(domain,team_id) VALUES(lower($1),$2)`, safe+".example.test", team)
	exec(`INSERT INTO paperboat.team_identity_state(team_id) VALUES($1)`, team)
	exec(`INSERT INTO paperboat.team_directory_group_roles(team_id,group_id,role) VALUES($1,$2,'member')`, team, "grp-"+safe)
	// The session insert seeds a default 'owner' policy row via trigger; adopt it.
	exec(`UPDATE paperboat.terminal_browser_access_policies SET audience='team', team_id=$2 WHERE terminal_session_id=$1`, session, team)
	exec(`INSERT INTO paperboat.browser_viewer_policies(resource_kind,resource_id,route_id,owner_account_id,audience,team_id,allowed_emails,generation,updated_by_account) VALUES('tunnel',$1,$2,$3,'team',$4,'{}',1,$3)`, tunnel, route, user, team)
	exec(`INSERT INTO paperboat.audit_stream_destinations(team_id,kind,url,secrets,cursor_sequence) VALUES($1,'https','https://example.test/hook',decode('00','hex'),0)`, team)
	exec(`INSERT INTO paperboat.team_buckets(team_id,endpoint,region,bucket,access_key_id,secret,checked_at) VALUES($1,'https://bucket.example.test','test-region',$2,'test-access',decode('00','hex'),now())`, team, "bkt-"+safe)
	exec(`INSERT INTO paperboat.workspace_policies(team_id,version,source,digest) VALUES($1,1,'{}','sha256:test')`, team)

	// Rows that cascade from the purged tables.
	exec(`INSERT INTO paperboat.team_resource_grants(team_id,account_id,resource_kind,resource_id,permission,generation,active) VALUES($1,$2,'env',$3,'read',1,true)`, team, user, envRes)
	exec(`INSERT INTO paperboat.team_terminal_session_grants(team_id,terminal_session_id,audience,role,generation,active) VALUES($1,$2,'all_members','viewer',1,true)`, team, session)
	exec(`INSERT INTO paperboat.team_machine_grants(team_id,machine_id,audience,capabilities,generation,active) VALUES($1,$2,'all_members',ARRAY['terminal'],1,true)`, team, machine)
	exec(`INSERT INTO paperboat.team_config_default_adoptions(team_id,account_id,default_version,repository_id) VALUES($1,$2,1,$3)`, team, aux, repo)
	exec(`INSERT INTO paperboat.workspace_policy_rules(policy_id,rule,deny,subject_kind,subject_id,target_kind,target_id,action,ports) SELECT id,0,false,'all','','all_machines','','terminal',int4range(1,100) FROM paperboat.workspace_policies WHERE team_id=$1`, team)
	exec(`INSERT INTO paperboat.workspace_policy_tags(policy_id,tag,machine_id) SELECT id,'tag:ci',$2 FROM paperboat.workspace_policies WHERE team_id=$1`, team, machine)
	exec(`INSERT INTO paperboat.workspace_policy_groups(policy_id,group_name,account_id) SELECT id,'ops',$2 FROM paperboat.workspace_policies WHERE team_id=$1`, team, user)
	exec(`INSERT INTO paperboat.workspace_policy_versions(policy_id,version,kind,source,digest) SELECT id,1,'saved','{}','sha256:test' FROM paperboat.workspace_policies WHERE team_id=$1`, team)
}

// teamRowCounts returns a SELECT count(*) query for every table
// insertDeletedTeamRows fills: each deletedTeamTables entry counts by its
// team_id column, and each cascading child counts directly or via the join
// needed to reach the team.
func teamRowCounts() map[string]string {
	out := map[string]string{}
	for _, table := range deletedTeamTables {
		out[table] = `SELECT count(*) FROM paperboat.` + table + ` WHERE team_id=$1`
	}
	out["team_resource_grants"] = `SELECT count(*) FROM paperboat.team_resource_grants WHERE team_id=$1`
	out["team_terminal_session_grants"] = `SELECT count(*) FROM paperboat.team_terminal_session_grants WHERE team_id=$1`
	out["team_machine_grants"] = `SELECT count(*) FROM paperboat.team_machine_grants WHERE team_id=$1`
	out["team_config_default_adoptions"] = `SELECT count(*) FROM paperboat.team_config_default_adoptions WHERE team_id=$1`
	out["workspace_policy_rules"] = `SELECT count(*) FROM paperboat.workspace_policy_rules r JOIN paperboat.workspace_policies p ON p.id=r.policy_id WHERE p.team_id=$1`
	out["workspace_policy_tags"] = `SELECT count(*) FROM paperboat.workspace_policy_tags t JOIN paperboat.workspace_policies p ON p.id=t.policy_id WHERE p.team_id=$1`
	out["workspace_policy_groups"] = `SELECT count(*) FROM paperboat.workspace_policy_groups g JOIN paperboat.workspace_policies p ON p.id=g.policy_id WHERE p.team_id=$1`
	out["workspace_policy_versions"] = `SELECT count(*) FROM paperboat.workspace_policy_versions v JOIN paperboat.workspace_policies p ON p.id=v.policy_id WHERE p.team_id=$1`
	return out
}

// deleteDeletedTeamFixture removes everything insertDeletedTeamRows inserted,
// including prerequisite rows, so the shared test database stays clean. The
// caller owns the teams row and the user row passed to insertDeletedTeamRows.
func deleteDeletedTeamFixture(exec func(string, ...any), team string) {
	safe := strings.ReplaceAll(strings.ToLower(team), "_", "-")
	aux := team + "-aux"
	machine := "mch-" + safe
	session := "ses-" + safe
	tunnel := "tun-" + safe
	route := "rte-" + safe
	repo := "repo-" + safe

	exec(`DELETE FROM paperboat.workspace_policy_rules WHERE policy_id IN (SELECT id FROM paperboat.workspace_policies WHERE team_id=$1)`, team)
	exec(`DELETE FROM paperboat.workspace_policy_tags WHERE policy_id IN (SELECT id FROM paperboat.workspace_policies WHERE team_id=$1)`, team)
	exec(`DELETE FROM paperboat.workspace_policy_groups WHERE policy_id IN (SELECT id FROM paperboat.workspace_policies WHERE team_id=$1)`, team)
	exec(`DELETE FROM paperboat.workspace_policy_versions WHERE policy_id IN (SELECT id FROM paperboat.workspace_policies WHERE team_id=$1)`, team)
	exec(`DELETE FROM paperboat.workspace_policies WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.team_resource_grants WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.team_terminal_session_grants WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.team_machine_grants WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.team_config_default_adoptions WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.terminal_browser_access_policies WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.browser_viewer_policies WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.team_resource_bindings WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.team_members WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.team_invitations WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.team_config_defaults WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.team_receipt_smtp WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.team_machine_requests WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.team_jit_requests WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.team_sso_domains WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.team_identity_state WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.team_directory_group_roles WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.audit_stream_destinations WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.team_buckets WHERE team_id=$1`, team)
	exec(`DELETE FROM paperboat.tunnel_routes WHERE id=$1`, route)
	exec(`DELETE FROM paperboat.tunnels WHERE id=$1`, tunnel)
	exec(`DELETE FROM paperboat.user_machine_terminal_sessions WHERE id=$1`, session)
	exec(`DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
	exec(`DELETE FROM paperboat.control_config_repositories WHERE id=$1`, repo)
	exec(`DELETE FROM paperboat.users WHERE id=$1`, aux)
	exec(`DELETE FROM paperboat.teams WHERE team_id=$1`, team)
}
