package usermachines

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestTerminalBrowserAccessPolicyPostgres(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	owner := "tbp_owner_" + suffix
	viewer := "tbp_viewer_" + suffix
	domainViewer := "tbp_domain_" + suffix
	subdomainViewer := "tbp_subdomain_" + suffix
	suffixViewer := "tbp_suffix_" + suffix
	unverifiedViewer := "tbp_unverified_" + suffix
	teamMember := "tbp_team_member_" + suffix
	outsider := "tbp_outsider_" + suffix
	team := "tbp-team-" + suffix
	unassociatedTeam := "tbp-unassociated-" + suffix
	machine := "tbp_machine_" + suffix
	session := "tbp_terminal_" + suffix
	otherSession := "tbp_other_terminal_" + suffix
	workspace := "tbp_workspace_" + suffix

	cleanup := func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_, _ = store.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.user_machine_terminal_sessions WHERE id IN ($1,$2)`, session, otherSession)
		_, _ = store.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		_, _ = store.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.teams WHERE team_id IN ($1,$2)`, team, unassociatedTeam)
		_, _ = store.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.users WHERE id IN ($1,$2,$3,$4,$5,$6,$7,$8)`, owner, viewer, domainViewer, subdomainViewer, suffixViewer, unverifiedViewer, teamMember, outsider)
	}
	t.Cleanup(cleanup)

	insert := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	insert(`INSERT INTO paperboat.users(id,workos_subject,primary_email,email_verified,status) VALUES
($1,$1,'owner@owner.invalid',true,'active'),
($2,$2,'viewer@company.com',true,'active'),
($3,$3,'domain@company.com',true,'active'),
($4,$4,'person@sub.company.com',true,'active'),
($5,$5,'person@notcompany.com',true,'active'),
($6,$6,'unverified@company.com',false,'active'),
($7,$7,'teammate@team.test',true,'active'),
($8,$8,'outsider@other.test',true,'active')`, owner, viewer, domainViewer, subdomainViewer, suffixViewer, unverifiedViewer, teamMember, outsider)
	insert(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1),($3,$2,1)`, team, owner, unassociatedTeam)
	insert(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active)
VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)`, team, owner, teamMember)
	now := time.Now().UTC()
	insert(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end)
VALUES($1,$2,'active','team-pro',2,$3,$4)`, workspace, team, now.Add(-time.Hour), now.Add(time.Hour))
	insert(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,description,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities)
VALUES($1,$2,$3,'browser-policy-host','Browser access test','linux','amd64','/workspace','online','occupied',true,ARRAY['terminal_host'],ARRAY['terminal_host'])`, machine, owner, "env_"+suffix)
	insert(`INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,owner_account,name,launch_cwd,desired_state,runtime_state)
VALUES($1,$2,$3,'shared','/workspace','open','running'),($4,$2,$3,'other','/workspace','open','running')`, session, machine, owner, otherSession)
	insert(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,terminal_process_generation)
VALUES($1,'terminal_session',$2,$3,1)`, team, session, owner)

	service := New(store, nil, Policy{}, nil)
	policy, err := service.TerminalBrowserAccessPolicy(ctx, owner, session)
	if err != nil || policy.Audience != "owner" || policy.Generation != 1 {
		t.Fatalf("new terminal default policy=%+v err=%v", policy, err)
	}
	if decision, err := service.ResolveTerminalBrowserRole(ctx, owner, session); err != nil || decision.Role != TerminalBrowserRoleOwner || decision.PolicyGeneration != 1 {
		t.Fatalf("owner default decision=%+v err=%v", decision, err)
	}
	if _, err = service.TerminalBrowserAccessPolicy(ctx, viewer, session); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("nonowner policy read: %v", err)
	}
	if _, err = service.SetTerminalBrowserAccessPolicy(ctx, viewer, session, TerminalBrowserAccessPolicyMutation{ExpectedGeneration: 1, Audience: "owner"}); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("nonowner policy mutation: %v", err)
	}

	policy, err = service.SetTerminalBrowserAccessPolicy(ctx, owner, session, TerminalBrowserAccessPolicyMutation{
		ExpectedGeneration: 1, Audience: "specific_emails", AllowedEmails: []string{"viewer@company.com", "unverified@company.com"},
	})
	if err != nil || policy.Generation != 2 {
		t.Fatalf("save exact email policy=%+v err=%v", policy, err)
	}
	if decision, err := service.ResolveTerminalBrowserRole(ctx, viewer, session); err != nil || decision.Role != TerminalBrowserRoleViewer {
		t.Fatalf("verified email viewer decision=%+v err=%v", decision, err)
	}
	if catalog, catalogErr := service.TerminalSessionCatalog(ctx, viewer); catalogErr != nil || len(catalog) != 1 || catalog[0].ID != session || catalog[0].Role != "viewer" || !catalog[0].BrowserOnly {
		t.Fatalf("browser-only shared terminal catalog=%+v err=%v", catalog, catalogErr)
	}
	if detail, detailErr := service.TerminalSessionSharing(ctx, viewer, session); detailErr != nil || !detail.BrowserOnly || detail.ParticipantsAvailable {
		t.Fatalf("browser-only shared terminal detail=%+v err=%v", detail, detailErr)
	}
	if _, err = service.ResolveTerminalBrowserRole(ctx, unverifiedViewer, session); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("unverified email gained access: %v", err)
	}
	if _, err = service.ResolveTerminalBrowserRole(ctx, viewer, otherSession); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("session-specific policy leaked to another terminal: %v", err)
	}

	interactive := TerminalBrowserAccessPolicyMutation{
		ExpectedGeneration: 2, Audience: "specific_emails", AllowedEmails: []string{"viewer@company.com", "unverified@company.com"},
		InteractiveEmails: []string{"viewer@company.com"},
	}
	if _, err = service.SetTerminalBrowserAccessPolicy(ctx, owner, session, interactive); !errors.Is(err, ErrTerminalBrowserInteractionConfirmationRequired) {
		t.Fatalf("interactive grant without confirmation: %v", err)
	}
	interactive.ConfirmInteractiveAccess = true
	policy, err = service.SetTerminalBrowserAccessPolicy(ctx, owner, session, interactive)
	if err != nil || policy.Generation != 3 {
		t.Fatalf("save confirmed interactive email=%+v err=%v", policy, err)
	}
	if decision, err := service.ResolveTerminalBrowserRole(ctx, viewer, session); err != nil || decision.Role != TerminalBrowserRoleInteractive {
		t.Fatalf("interactive email decision=%+v err=%v", decision, err)
	}
	interactive.ExpectedGeneration = 3
	if _, err = service.SetTerminalBrowserAccessPolicy(ctx, owner, session, interactive); err != nil {
		t.Fatalf("idempotent policy save: %v", err)
	} else if current, readErr := service.TerminalBrowserAccessPolicy(ctx, owner, session); readErr != nil || current.Generation != 3 {
		t.Fatalf("unchanged policy bumped generation: policy=%+v err=%v", current, readErr)
	}

	insert(`UPDATE paperboat.users SET primary_email='renamed@other.test',email_verified=true WHERE id=$1`, viewer)
	if _, err = service.ResolveTerminalBrowserRole(ctx, viewer, session); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("stale email kept exact access: %v", err)
	}
	if catalog, catalogErr := service.TerminalSessionCatalog(ctx, viewer); catalogErr != nil || len(catalog) != 0 {
		t.Fatalf("email change retained browser-only terminal catalog=%+v err=%v", catalog, catalogErr)
	}
	insert(`UPDATE paperboat.users SET primary_email='viewer@company.com',email_verified=false WHERE id=$1`, viewer)
	if _, err = service.ResolveTerminalBrowserRole(ctx, viewer, session); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("email verification removal kept access: %v", err)
	}
	insert(`UPDATE paperboat.users SET email_verified=true WHERE id=$1`, viewer)

	domainPolicy := TerminalBrowserAccessPolicyMutation{ExpectedGeneration: 3, Audience: "specific_emails", AllowedEmails: []string{"*@company.com"}, InteractiveEmails: []string{"viewer@company.com"}, ConfirmInteractiveAccess: true}
	policy, err = service.SetTerminalBrowserAccessPolicy(ctx, owner, session, domainPolicy)
	if err != nil || policy.Generation != 4 {
		t.Fatalf("save domain policy=%+v err=%v", policy, err)
	}
	for _, account := range []string{domainViewer} {
		if decision, resolveErr := service.ResolveTerminalBrowserRole(ctx, account, session); resolveErr != nil || decision.Role != TerminalBrowserRoleViewer {
			t.Errorf("exact domain account %s decision=%+v err=%v", account, decision, resolveErr)
		}
	}
	if catalog, catalogErr := service.TerminalSessionCatalog(ctx, domainViewer); catalogErr != nil || len(catalog) != 1 || catalog[0].ID != session || !catalog[0].BrowserOnly {
		t.Fatalf("domain browser-only terminal catalog=%+v err=%v", catalog, catalogErr)
	}
	for _, account := range []string{subdomainViewer, suffixViewer, unverifiedViewer} {
		if _, resolveErr := service.ResolveTerminalBrowserRole(ctx, account, session); !errors.Is(resolveErr, ErrTerminalSessionNotFound) {
			t.Errorf("out-of-domain/unverified account %s access err=%v", account, resolveErr)
		}
	}
	if decision, err := service.ResolveTerminalBrowserRole(ctx, viewer, session); err != nil || decision.Role != TerminalBrowserRoleInteractive {
		t.Fatalf("explicit domain interact decision=%+v err=%v", decision, err)
	}

	teamPolicy := TerminalBrowserAccessPolicyMutation{
		ExpectedGeneration: 4, Audience: "team", TeamID: team, InteractiveEmails: []string{"teammate@team.test"}, ConfirmInteractiveAccess: true,
	}
	policy, err = service.SetTerminalBrowserAccessPolicy(ctx, owner, session, teamPolicy)
	if err != nil || policy.Generation != 5 {
		t.Fatalf("save current team policy=%+v err=%v", policy, err)
	}
	if decision, err := service.ResolveTerminalBrowserRole(ctx, teamMember, session); err != nil || decision.Role != TerminalBrowserRoleInteractive || decision.TeamID != team || decision.WorkspaceID != "personal" {
		t.Fatalf("current team member decision=%+v err=%v", decision, err)
	}
	if catalog, catalogErr := service.TerminalSessionCatalog(ctx, teamMember); catalogErr != nil || len(catalog) != 1 || catalog[0].ID != session || catalog[0].Role != "interactive" || !catalog[0].BrowserOnly {
		t.Fatalf("team browser-only terminal catalog=%+v err=%v", catalog, catalogErr)
	}
	if _, err = service.ResolveTerminalBrowserRole(ctx, outsider, session); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("nonmember gained team terminal view: %v", err)
	}
	teamPolicy.ExpectedGeneration = 5
	teamPolicy.TeamID = unassociatedTeam
	if _, err = service.SetTerminalBrowserAccessPolicy(ctx, owner, session, teamPolicy); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("arbitrary owner-managed team association accepted: %v", err)
	}
	if decision, err := service.ResolveTerminalBrowserRole(ctx, owner, session); err != nil || decision.Role != TerminalBrowserRoleOwner {
		t.Fatalf("owner lost access after policy changes decision=%+v err=%v", decision, err)
	}

	// The browser policy itself grants this exact terminal; native binding is not a prerequisite.
	insert(`DELETE FROM paperboat.team_resource_bindings WHERE team_id=$1 AND resource_kind='terminal_session' AND resource_id=$2`, team, session)
	if decision, err := service.ResolveTerminalBrowserRole(ctx, teamMember, session); err != nil || decision.Role != TerminalBrowserRoleInteractive || decision.BindingGeneration != 0 {
		t.Fatalf("browser-only unbound team decision=%+v err=%v", decision, err)
	}
	if catalog, err := service.TerminalSessionCatalog(ctx, teamMember); err != nil || len(catalog) != 1 || catalog[0].ID != session || !catalog[0].BrowserOnly {
		t.Fatalf("unbound browser team catalog=%+v err=%v", catalog, err)
	}
	insert(`UPDATE paperboat.team_members SET active=false,membership_generation=membership_generation+1 WHERE team_id=$1 AND account_id=$2`, team, owner)
	if _, err := service.ResolveTerminalBrowserRole(ctx, teamMember, session); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("departed owner kept team authority: %v", err)
	}

	var nativeTerminalGrants, nativeMachineGrants int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.team_terminal_session_grants WHERE terminal_session_id=$1`, session).Scan(&nativeTerminalGrants); err != nil {
		t.Fatal(err)
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.team_machine_grants WHERE machine_id=$1`, machine).Scan(&nativeMachineGrants); err != nil {
		t.Fatal(err)
	}
	if nativeTerminalGrants != 0 || nativeMachineGrants != 0 {
		t.Fatalf("browser policy created native grants: terminal=%d machine=%d", nativeTerminalGrants, nativeMachineGrants)
	}
	if len(policy.AllowedEmails) != 0 {
		t.Fatalf("team policy retained prior email domain: %+v", policy)
	}
}
