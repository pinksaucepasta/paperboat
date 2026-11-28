package browseraccess

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// The TestNoChange tests pin behaviour that email allow lists must leave alone. They use only what existed before the
// lists, so the same file passes on the code from before them.

func (f *privatePolicyFixture) paidTeam(name string, members ...string) string {
	f.t.Helper()
	team := "team-" + name + "-" + f.suffix
	f.t.Cleanup(func() {
		_, _ = f.store.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
	})
	f.exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, f.owner)
	for _, member := range append([]string{f.owner}, members...) {
		f.exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, member)
	}
	f.exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',3,$3,$4)`, "ws_"+name+"_"+f.suffix, team, f.now.Add(-time.Hour), f.now.Add(time.Hour))
	return team
}

// pro makes the fixture owner a Pro account: its public routes stay fully public (no Free plan page-load gate), so
// the rules these tests pin are the ones paid accounts keep.
func (f *privatePolicyFixture) pro() {
	f.t.Helper()
	f.t.Cleanup(func() {
		_, _ = f.store.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, "personal:"+f.owner)
	})
	f.exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,provider_customer_id,provider_subscription_id,product_code,state,quantity,period_start,period_end) VALUES($1,$2,$3,$4,'pro-month','active',1,$5,$6)`, "personal:"+f.owner, f.owner, "cus_pro_"+f.suffix, "sub_pro_"+f.suffix, f.now.Add(-time.Hour), f.now.Add(time.Hour))
}

func (f *privatePolicyFixture) bind(team, kind, id string, users ...string) {
	f.t.Helper()
	f.exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active) VALUES($1,$2,$3,$4,true) ON CONFLICT DO NOTHING`, team, kind, id, f.owner)
	for _, user := range users {
		f.exec(`INSERT INTO paperboat.team_resource_grants(team_id,account_id,resource_kind,resource_id,permission,generation,active) VALUES($1,$2,$3,$4,'use',1,true)`, team, user, kind, id)
	}
}

func (f *privatePolicyFixture) edgeMode(route string) (mode string) {
	f.t.Helper()
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT effective_access_mode FROM paperboat.current_tunnel_route_viewer_policy WHERE route_id=$1`, route).Scan(&mode); err != nil {
		f.t.Fatal(err)
	}
	return mode
}

func (f *privatePolicyFixture) unconfigured(kind, id, route, native string) {
	f.t.Helper()
	policy, err := f.svc.GetViewerPolicy(f.ctx, f.owner, kind, id, route)
	if err != nil || policy.Configured || policy.Generation != 0 || policy.Audience != "" || len(policy.AllowedEmails) != 0 || policy.NativeAccessMode != native {
		f.t.Fatalf("%s %s without a saved policy = %#v, %v", native, kind, policy, err)
	}
}

// A public address needs no browser sign-in, so the sign-in service refuses to make one for anybody: as a denial
// without a saved policy, and as not authorized with a saved public one.
func (f *privatePolicyFixture) takesNoSignIn(host string, want error) {
	f.t.Helper()
	for _, user := range []string{f.owner, f.listed, f.other} {
		if _, err := f.signIn(user, host); !errors.Is(err, want) {
			f.t.Errorf("sign-in of %s on a public address = %v, want %v", user, err, want)
		}
	}
}

// Policies written by the earlier server read back field for field and admit the same people.
func TestNoChangeSavedViewerPoliciesReadBackIdenticallyPostgres(t *testing.T) {
	f := newPrivatePolicyFixture(t)
	f.pro()
	team := f.paidTeam("saved", f.other)
	saved := f.now.Add(-24 * time.Hour)
	for _, row := range []struct {
		audience, team, domain string
		emails                 []string
		edge                   string
		admits                 map[string]bool
	}{
		{audience: AudiencePublic, edge: "public"},
		{audience: AudienceOwner, edge: "private", admits: map[string]bool{f.owner: true, f.listed: false, f.domain: false, f.other: false}},
		{audience: AudienceSpecificEmails, emails: []string{"guest@partner.test", "anyone@sub.company.test", "second@company.test"}, edge: "private",
			admits: map[string]bool{f.owner: true, f.listed: true, f.subdomain: true, f.domain: false, f.other: false, f.unverified: false}},
		{audience: AudienceEmailDomain, domain: "company.test", edge: "private",
			admits: map[string]bool{f.owner: true, f.domain: true, f.subdomain: false, f.listed: false, f.unverified: false}},
		{audience: AudienceTeam, team: team, edge: "private", admits: map[string]bool{f.owner: true, f.other: true, f.listed: false}},
	} {
		t.Run(row.audience, func(t *testing.T) {
			g := *f
			g.t = t
			id, route, host := g.tunnel("saved"+row.audience, "public")
			if row.team != "" {
				g.bind(row.team, "tunnel", id)
			}
			emails := row.emails
			if emails == nil {
				emails = []string{}
			}
			g.exec(`INSERT INTO paperboat.browser_viewer_policies(resource_kind,resource_id,route_id,owner_account_id,audience,team_id,allowed_emails,email_domain,generation,updated_by_account,created_at,updated_at) VALUES('tunnel',$1,$2,$3,$4,NULLIF($5,''),$6,NULLIF($7,''),7,$3,$8,$8)`, id, route, g.owner, row.audience, row.team, emails, row.domain, saved)
			policy, err := g.svc.GetViewerPolicy(g.ctx, g.owner, "tunnel", id, route)
			if err != nil || !policy.Configured || policy.Audience != row.audience || policy.TeamID != row.team || policy.EmailDomain != row.domain ||
				!slices.Equal(policy.AllowedEmails, emails) || policy.Generation != 7 || !policy.UpdatedAt.Equal(saved) || policy.OwnerAccountID != g.owner || policy.NativeAccessMode != "public" {
				t.Fatalf("saved policy read back = %#v, %v", policy, err)
			}
			if mode := g.edgeMode(route); mode != row.edge {
				t.Fatalf("edge mode = %q, want %q", mode, row.edge)
			}
			if row.audience == AudiencePublic {
				g.takesNoSignIn(host, ErrNotAuthorized)
			}
			g.admits(host, row.admits)
		})
	}
}

// Without a saved policy, public, private and team previews and tunnels admit whom they admitted before.
func TestNoChangeAccessModesWithoutAnAllowListPostgres(t *testing.T) {
	f := newPrivatePolicyFixture(t)
	f.pro()
	team := f.paidTeam("modes", f.domain, f.other)
	machineCredential := func(kind, account, host, id, route string) error {
		_, err := f.svc.IssueMachineCredential(f.ctx, MachineCredentialRequest{AccountID: account, Host: host, ResourceKind: kind, ResourceID: id, RouteID: route, Action: "use", TTL: time.Minute})
		return err
	}
	ownerOnly := map[string]bool{f.owner: true, f.listed: false, f.domain: false, f.other: false}
	teamUse := map[string]bool{f.domain: true, f.other: false, f.listed: false}

	for _, mode := range []string{"public", "private", "team"} {
		previewID, previewHost := f.preview("nolist"+mode, mode, "")
		tunnelID, route, tunnelHost := f.tunnel("nolist"+mode, mode)
		f.unconfigured("preview", previewID, previewID, mode)
		f.unconfigured("tunnel", tunnelID, route, mode)
		if got := f.edgeMode(route); got != mode {
			t.Fatalf("edge mode of a %s tunnel without a policy = %q", mode, got)
		}
		want := map[string]bool{}
		switch mode {
		case "public":
			f.takesNoSignIn(previewHost, ErrAccessDenied)
			f.takesNoSignIn(tunnelHost, ErrAccessDenied)
		case "private":
			want = ownerOnly
		case "team":
			f.bind(team, "preview", previewID, f.domain)
			f.bind(team, "tunnel", tunnelID, f.domain)
			want = teamUse
		}
		f.admits(previewHost, want)
		f.admits(tunnelHost, want)
		if mode == "private" {
			if err := machineCredential("preview", f.owner, previewHost, previewID, previewID); err != nil {
				t.Fatalf("owner machine credential on a private preview = %v", err)
			}
			if err := machineCredential("preview", f.other, previewHost, previewID, previewID); !errors.Is(err, ErrNotAuthorized) {
				t.Fatalf("stranger machine credential on a private preview = %v", err)
			}
		}
		if f.policies(previewID) != 0 || f.policies(tunnelID) != 0 {
			t.Fatalf("signing in saved a policy on a %s resource", mode)
		}
	}

	// A Team's own private preview still follows the Team's grants.
	owned, ownedHost := f.preview("nolistowned", "private", team)
	f.bind(team, "preview", owned, f.domain)
	f.unconfigured("preview", owned, owned, "private")
	f.admits(ownedHost, teamUse)
}

// Pro and Team public previews and tunnels stay public with no policy, and keep the choices they had.
func TestNoChangePaidPublicPublicationsPostgres(t *testing.T) {
	f := newPrivatePolicyFixture(t)
	t.Cleanup(func() {
		_, _ = f.store.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, "personal:"+f.owner)
	})
	f.exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,provider_customer_id,provider_subscription_id,product_code,state,quantity,period_start,period_end) VALUES($1,$2,$3,$4,'pro-month','active',1,$5,$6)`, "personal:"+f.owner, f.owner, "cus_nc_"+f.suffix, "sub_nc_"+f.suffix, f.now.Add(-time.Hour), f.now.Add(time.Hour))
	team := f.paidTeam("public")
	for _, plan := range []struct{ name, team string }{{"pro", ""}, {"team", team}} {
		previewID, _ := f.preview("pub"+plan.name, "public", plan.team)
		tunnelID, route, _ := f.tunnelOwned("pub"+plan.name, "public", plan.team)
		f.unconfigured("preview", previewID, previewID, "public")
		f.unconfigured("tunnel", tunnelID, route, "public")
		var previewMode string
		if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT access_mode FROM paperboat.preview_leases WHERE id=$1`, previewID).Scan(&previewMode); err != nil || previewMode != "public" {
			t.Fatalf("%s public preview = %q, %v", plan.name, previewMode, err)
		}
		if got := f.edgeMode(route); got != "public" {
			t.Fatalf("%s public tunnel edge mode = %q", plan.name, got)
		}
		if _, err := f.set("preview", previewID, previewID, AudienceOwner, 0); !errors.Is(err, ErrPolicyEdgeTransition) {
			t.Fatalf("%s public preview limited to its owner = %v, want ErrPolicyEdgeTransition", plan.name, err)
		}
		for generation, step := range []struct {
			audience, edge string
			emails         []string
		}{
			{audience: AudienceOwner, edge: "private"},
			{audience: AudienceSpecificEmails, edge: "private", emails: []string{"guest@partner.test"}},
			{audience: AudiencePublic, edge: "public"},
		} {
			if _, err := f.set("tunnel", tunnelID, route, step.audience, int64(generation), step.emails...); err != nil {
				t.Fatalf("%s public tunnel set to %s = %v", plan.name, step.audience, err)
			}
			if got := f.edgeMode(route); got != step.edge {
				t.Fatalf("%s public tunnel set to %s has edge mode %q", plan.name, step.audience, got)
			}
		}
	}
}

// A private or team preview or tunnel is never made public, or opened wider, through a viewer policy.
func TestNoChangePrivateIsNeverWidenedPostgres(t *testing.T) {
	f := newPrivatePolicyFixture(t)
	team := f.paidTeam("widen", f.other)
	for _, mode := range []string{"private", "team"} {
		previewID, previewHost := f.preview("widen"+mode, mode, "")
		tunnelID, route, tunnelHost := f.tunnel("widen"+mode, mode)
		for _, target := range []struct{ kind, id, route, host string }{{"preview", previewID, previewID, previewHost}, {"tunnel", tunnelID, route, tunnelHost}} {
			f.bind(team, target.kind, target.id)
			if _, err := f.set(target.kind, target.id, target.route, AudiencePublic, 0); !errors.Is(err, ErrPolicyWidening) {
				t.Fatalf("%s %s made public = %v, want ErrPolicyWidening", mode, target.kind, err)
			}
			if _, err := f.svc.SetViewerPolicy(f.ctx, SetViewerPolicyRequest{ActorAccountID: f.owner, ResourceKind: target.kind, ResourceID: target.id, RouteID: target.route, Audience: AudienceTeam, TeamID: team}); !errors.Is(err, ErrPolicyWidening) {
				t.Fatalf("%s %s opened to a team = %v, want ErrPolicyWidening", mode, target.kind, err)
			}
			if f.policies(target.id) != 0 {
				t.Fatalf("a refused policy was saved on a %s %s", mode, target.kind)
			}
			f.admits(target.host, map[string]bool{f.listed: false, f.domain: false})
		}
		if got := f.edgeMode(route); got != mode {
			t.Fatalf("edge mode of a %s tunnel after refused widening = %q", mode, got)
		}
	}
}
