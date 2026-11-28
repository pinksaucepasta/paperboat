package browseraccess

import (
	"errors"
	"testing"
	"time"
)

// A live public preview takes, changes and clears a list of viewers in place, on Free, Pro and Team: while a list
// is saved only signed-in listed viewers (and the owner) get a browser grant, issued restricted ("private"), and
// clearing it back to everyone makes the address public again with no sign-in. Before P3b the first set was
// refused with ErrPolicyEdgeTransition.
func TestLivePublicPreviewTakesChangesAndClearsAListPostgres(t *testing.T) {
	f := newPrivatePolicyFixture(t)
	t.Cleanup(func() {
		_, _ = f.store.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, "personal:"+f.owner)
	})
	team := f.paidTeam("publist")
	for _, plan := range []struct{ name, team string }{{"free", ""}, {"pro", ""}, {"team", team}} {
		if plan.name == "pro" {
			f.exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,provider_customer_id,provider_subscription_id,product_code,state,quantity,period_start,period_end) VALUES($1,$2,$3,$4,'pro-month','active',1,$5,$6)`, "personal:"+f.owner, f.owner, "cus_pl_"+f.suffix, "sub_pl_"+f.suffix, f.now.Add(-time.Hour), f.now.Add(time.Hour))
		}
		id, host := f.preview("pl"+plan.name, "public", plan.team)
		if plan.name == "free" {
			// P4: on Free (Paperboat edge) browser page loads sign in; with no list only the owner gets a grant.
			f.admits(host, map[string]bool{f.owner: true, f.listed: false, f.other: false})
		} else {
			f.takesNoSignIn(host, ErrAccessDenied)
		}

		// Set: one address and a domain.
		policy, err := f.set("preview", id, id, AudienceSpecificEmails, 0, "guest@partner.test")
		if err != nil {
			t.Fatalf("%s: list on a live public preview = %v", plan.name, err)
		}
		if policy.NativeAccessMode != "public" || policy.Generation != 1 {
			t.Fatalf("%s: saved policy = %#v", plan.name, policy)
		}
		f.admits(host, map[string]bool{f.owner: true, f.listed: true, f.domain: false, f.other: false, f.unverified: false})
		token, err := f.signIn(f.listed, host)
		if err != nil {
			t.Fatal(err)
		}
		grant, err := f.svc.Authorize(f.ctx, AuthorizeRequest{Token: token, Host: host, ResourceKind: "preview", ResourceID: id, RouteID: id})
		if err != nil || grant.AccessMode != "private" || grant.ViewerPolicyGeneration != 1 {
			t.Fatalf("%s: listed viewer's grant = %#v, %v (want a restricted grant at policy generation 1)", plan.name, grant, err)
		}

		// Change: to a domain; the earlier viewer's session ends.
		if _, err = f.set("preview", id, id, AudienceSpecificEmails, 1, "*@company.test"); err != nil {
			t.Fatalf("%s: change list = %v", plan.name, err)
		}
		if _, err = f.svc.Authorize(f.ctx, AuthorizeRequest{Token: token, Host: host, ResourceKind: "preview", ResourceID: id, RouteID: id}); !errors.Is(err, ErrNotAuthorized) {
			t.Fatalf("%s: a viewer taken off the list kept the session: %v", plan.name, err)
		}
		f.admits(host, map[string]bool{f.owner: true, f.listed: false, f.domain: true, f.subdomain: false, f.other: false})

		// Clear: back to everyone; no sign-in again. On Free (P4) "everyone" is refused: browser visitors must sign in.
		if plan.name == "free" {
			if _, err = f.set("preview", id, id, AudiencePublic, 2); !errors.Is(err, ErrPlanBrowserPublic) {
				t.Fatalf("free: clear list = %v, want ErrPlanBrowserPublic", err)
			}
			continue
		}
		if _, err = f.set("preview", id, id, AudiencePublic, 2); err != nil {
			t.Fatalf("%s: clear list = %v", plan.name, err)
		}
		f.takesNoSignIn(host, ErrNotAuthorized)
		var mode string
		if err = f.store.SQL().QueryRowContext(f.ctx, `SELECT access_mode FROM paperboat.preview_leases WHERE id=$1`, id).Scan(&mode); err != nil || mode != "public" {
			t.Fatalf("%s: preview published mode changed to %q, %v", plan.name, mode, err)
		}
	}
}
