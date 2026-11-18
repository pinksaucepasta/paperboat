package operatoractions

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"testing"
	"time"

	"github.com/google/uuid"
)

// An operator gives a workspace extra bandwidth or a limit override through a receipt: exact replay, a conflicting reuse of
// the key, admin only, the grant and its audit event in one transaction, revoke by grant id, and refusals of what is invalid.
func TestEntitlementGrantAndRevokeThroughReceipts(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	s := NewService(store)
	x := fmt.Sprint(time.Now().UnixNano())
	admin, support := "operator_"+uuid.NewString(), "operator_"+uuid.NewString()
	user, team := "ent_user_"+x, "ent-team-"+x
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','x','active','admin'),($2,$2||'@test.invalid','x','active','support')`, admin, support)
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status,role) VALUES($1,$1,$1||'@test.invalid','active','user')`, user)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, user)
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspace_grants WHERE workspace_id IN ($1,$2)`, "personal:"+user, "team:"+team)
	})
	personal, teamWorkspace := "personal:"+user, "team:"+team
	run := func(actor, key, kind, target string, in Input) (Result, error) {
		return s.Execute(ctx, actor, key, kind, target, "case:test", in)
	}
	running := func(workspace, kind string) (amount int64, n int) {
		t.Helper()
		rows, err := store.SQL().QueryContext(ctx, `SELECT amount FROM paperboat.billing_workspace_grants WHERE workspace_id=$1 AND kind=$2 AND revoked_at IS NULL AND (ends_at IS NULL OR ends_at>now())`, workspace, kind)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			n++
			if err := rows.Scan(&amount); err != nil {
				t.Fatal(err)
			}
		}
		return
	}

	// Only an admin; refusals of invalid grants; none of them leaves a row.
	ends := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := run(support, "g-support-"+x, "entitlement_grant", personal, Input{Entitlement: "bandwidth", Amount: 1e9}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin granted bandwidth: %v", err)
	}
	for name, c := range map[string]struct {
		target string
		in     Input
		want   error
	}{
		"revision":        {personal, Input{Revision: 1, Entitlement: "bandwidth", Amount: 1}, ErrInvalid},
		"no entitlement":  {personal, Input{Amount: 5}, ErrInvalid},
		"unknown kind":    {personal, Input{Entitlement: "coffee", Amount: 5}, ErrInvalid},
		"seats personal":  {personal, Input{Entitlement: "seats", Amount: 5}, ErrInvalid},
		"zero bandwidth":  {personal, Input{Entitlement: "bandwidth", Amount: 0}, ErrInvalid},
		"seats too many":  {teamWorkspace, Input{Entitlement: "seats", Amount: teams.MaximumMembers + 1}, ErrInvalid},
		"zero limit":      {personal, Input{Entitlement: "machines", Amount: 0}, ErrInvalid},
		"bad date":        {personal, Input{Entitlement: "bandwidth", Amount: 1, EndsAt: "tomorrow"}, ErrInvalid},
		"past date":       {personal, Input{Entitlement: "bandwidth", Amount: 1, EndsAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}, ErrInvalid},
		"plan fields":     {personal, Input{Entitlement: "bandwidth", Amount: 1, ProductCode: "pro"}, ErrInvalid},
		"not a workspace": {"workspace_" + x, Input{Entitlement: "bandwidth", Amount: 1}, ErrInvalid},
		"unknown account": {"personal:nobody_" + x, Input{Entitlement: "bandwidth", Amount: 1}, ErrNotFound},
		"unknown team":    {"team:nobody_" + x, Input{Entitlement: "machines", Amount: 5}, ErrNotFound},
	} {
		if _, err := run(admin, "g-bad-"+name+x, "entitlement_grant", c.target, c.in); !errors.Is(err, c.want) {
			t.Fatalf("%s: %v, want %v", name, err, c.want)
		}
	}
	if _, n := running(personal, "bandwidth"); n != 0 {
		t.Fatal("a refused grant left a row")
	}

	granted, err := run(admin, "g-bw-"+x, "entitlement_grant", personal, Input{Entitlement: "bandwidth", Amount: 50_000_000_000, EndsAt: ends})
	if err != nil || granted.State != "granted" || granted.GrantID == "" || granted.ExpiresAt == nil {
		t.Fatalf("grant: %+v %v", granted, err)
	}
	if replay, err := run(admin, "g-bw-"+x, "entitlement_grant", personal, Input{Entitlement: "bandwidth", Amount: 50_000_000_000, EndsAt: ends}); err != nil || replay.GrantID != granted.GrantID {
		t.Fatalf("replay must return the same grant: %+v %v", replay, err)
	}
	if _, err = run(admin, "g-bw-"+x, "entitlement_grant", personal, Input{Entitlement: "bandwidth", Amount: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("one key, two requests: %v", err)
	}
	if amount, n := running(personal, "bandwidth"); n != 1 || amount != 50_000_000_000 {
		t.Fatalf("running grants %d of %d bytes (a replay must not add one)", n, amount)
	}
	var events int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE event_type='operator.entitlement_grant' AND resource_id=$1 AND resource_type='billing_workspace' AND metadata->>'entitlement'='bandwidth' AND metadata->>'grant_id'=$2`, personal, granted.GrantID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("audit events=%d %v", events, err)
	}

	// A limit override on a team; a newer one for the kind replaces it; a revoke needs a running grant of that workspace.
	first, err := run(admin, "g-m1-"+x, "entitlement_grant", teamWorkspace, Input{Entitlement: "machines", Amount: 200})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run(admin, "g-m2-"+x, "entitlement_grant", teamWorkspace, Input{Entitlement: "machines", Amount: 300}); err != nil {
		t.Fatal(err)
	}
	if amount, n := running(teamWorkspace, "machines"); n != 1 || amount != 300 {
		t.Fatalf("running overrides %d of %d", n, amount)
	}
	if _, err = run(admin, "r-old-"+x, "entitlement_revoke", teamWorkspace, Input{GrantID: first.GrantID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking a replaced override: %v", err)
	}
	if _, err = run(admin, "r-other-"+x, "entitlement_revoke", personal, Input{GrantID: first.GrantID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking another workspace's grant: %v", err)
	}
	if _, err = run(support, "r-support-"+x, "entitlement_revoke", personal, Input{GrantID: granted.GrantID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin revoked: %v", err)
	}
	if _, err = run(admin, "r-bad-"+x, "entitlement_revoke", personal, Input{GrantID: granted.GrantID, Amount: 5}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("grant fields on a revoke: %v", err)
	}
	revoked, err := run(admin, "r-bw-"+x, "entitlement_revoke", personal, Input{GrantID: granted.GrantID})
	if err != nil || revoked.State != "revoked" || revoked.GrantID != granted.GrantID {
		t.Fatalf("revoke: %+v %v", revoked, err)
	}
	if _, n := running(personal, "bandwidth"); n != 0 {
		t.Fatal("a revoked grant still runs")
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE event_type='operator.entitlement_revoke' AND resource_id=$1 AND metadata->>'grant_id'=$2`, personal, granted.GrantID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("revoke audit events=%d %v", events, err)
	}
}
