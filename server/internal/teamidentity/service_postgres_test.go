package teamidentity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

type fakeWorkOS struct {
	mu    sync.Mutex
	state OrganizationState
	orgs  int
}

func (f *fakeWorkOS) CreateOrganization(context.Context, string, string, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orgs++
	return "org_test", nil
}
func (f *fakeWorkOS) DeleteOrganization(context.Context, string) error { return nil }
func (f *fakeWorkOS) PortalLink(_ context.Context, org, intent, _ string) (string, error) {
	return "https://setup.example.test/" + org + "/" + intent, nil
}
func (f *fakeWorkOS) Events(context.Context, string, int) ([]Event, string, error) {
	return nil, "", nil
}
func (f *fakeWorkOS) Organization(context.Context, string) (OrganizationState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

type fakeSeats struct{ calls []int }

func (f *fakeSeats) EnsureSeats(_ context.Context, _, _ string, quantity int, _ string) error {
	f.calls = append(f.calls, quantity)
	return nil
}

func testStore(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	return store
}

// PLAN F19-02..06 end to end against PostgreSQL with a fake WorkOS.
func TestPostgresTeamSSOAndDirectorySync(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	domain := "acme" + x + ".test"
	owner, alice, bob, dave := "sso_owner_"+x, "sso_alice_"+x, "sso_bob_"+x, "sso_dave_"+x
	team := "sso-" + x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status,email_verified) VALUES($1,$1,'owner@'||$5,'active',true),($2,$2,'alice@'||$5,'active',true),($3,$3,'bob@'||$5,'active',true),($4,$4,'dave@'||$5,'active',true)`, owner, alice, bob, dave, domain)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation,display_name) VALUES($1,$2,1,'SSO team')`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, team, owner)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,quantity,period_start,period_end) VALUES('team:'||$1,$1,'active',10,now()-interval '1 day',now()+interval '30 days')`, team)
	exec(`DELETE FROM paperboat.users WHERE id=$1`, bob) // bob has no account yet
	fake := &fakeWorkOS{}
	seats := &fakeSeats{}
	s := New(store, fake, config.Default().Business, config.Default().Mail.InvitationLifetimeSeconds, "https://console.example.test/team", seats)
	tx := func(fn func(context.Context, *db.Tx) error) error { return store.InTx(ctx, fn) }

	if _, err := s.PortalLink(ctx, alice, team, "sso"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-member portal: %v", err)
	}
	link, err := s.PortalLink(ctx, owner, team, "sso")
	if err != nil || link != "https://setup.example.test/org_test/sso" {
		t.Fatalf("portal link=%q err=%v", link, err)
	}
	fake.state = OrganizationState{
		Domains:     []Domain{{Domain: domain, State: "verified"}, {Domain: "other.test", State: "pending"}},
		Connections: []Connection{{ID: "conn_1", Type: "OktaSAML", State: "active"}},
		Directory:   &Directory{ID: "directory_1", Type: "okta scim v2.0", State: "linked"},
		Groups:      []DirectoryGroup{{ID: "g_admins", Name: "Admins"}},
		Users: []DirectoryUser{
			{ID: "du_alice", Email: "alice@" + domain, Active: true, Groups: []string{"g_admins"}},
			{ID: "du_bob", Email: "bob@" + domain, Active: true},
			{ID: "du_owner", Email: "owner@" + domain, Active: true},
		},
	}
	if _, err = s.UpdateDirectory(ctx, owner, team, DirectorySettings{GroupRoles: map[string]string{"g_admins": "admin"}}); err != nil {
		t.Fatal(err)
	}
	if err = s.Reconcile(ctx, team); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(ctx, owner, team)
	if err != nil || len(status.SSO.Domains) != 2 || len(status.Directory.Groups) != 1 || status.Directory.Groups[0].Role != "admin" || status.Directory.Users != 3 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	var role string
	var managed bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT role,directory_user_id IS NOT NULL FROM paperboat.team_members WHERE team_id=$1 AND account_id=$2 AND active`, team, alice).Scan(&role, &managed); err != nil || role != "admin" || !managed {
		t.Fatalf("alice role=%s managed=%v err=%v", role, managed, err)
	}
	var held int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.team_invitations WHERE team_id=$1 AND recipient_email=$2 AND directory_user_id='du_bob' AND accepted_at IS NULL AND cancelled_at IS NULL`, team, "bob@"+domain).Scan(&held); err != nil || held != 1 {
		t.Fatalf("bob held invitation=%d err=%v", held, err)
	}
	// The held invitation has the normal lifetime (readers scan it; 'infinity' broke the team's
	// invitation list live) and follows the group mapping on the next reconcile.
	heldInvitation := func() (string, time.Time) {
		t.Helper()
		var role string
		var expires time.Time
		if err := store.SQL().QueryRowContext(ctx, `SELECT role,expires_at FROM paperboat.team_invitations WHERE team_id=$1 AND directory_user_id='du_bob' AND accepted_at IS NULL AND cancelled_at IS NULL`, team).Scan(&role, &expires); err != nil {
			t.Fatal(err)
		}
		return role, expires
	}
	lifetime := time.Duration(config.Default().Mail.InvitationLifetimeSeconds) * time.Second
	if role, expires := heldInvitation(); role != "member" || time.Until(expires) < lifetime-time.Hour || time.Until(expires) > lifetime+time.Hour {
		t.Fatalf("bob held invitation role=%s expires=%v", role, expires)
	}
	fake.state.Groups = append(fake.state.Groups, DirectoryGroup{ID: "g_fin", Name: "Finance"})
	fake.state.Users[1].Groups = []string{"g_fin"}
	if _, err = s.UpdateDirectory(ctx, owner, team, DirectorySettings{GroupRoles: map[string]string{"g_fin": "billing"}}); err != nil {
		t.Fatal(err)
	}
	if err = s.Reconcile(ctx, team); err != nil {
		t.Fatal(err)
	}
	if role, _ := heldInvitation(); role != "billing" {
		t.Fatalf("bob held invitation role after mapping = %s", role)
	}
	if len(seats.calls) != 1 || seats.calls[0] != 3 {
		t.Fatalf("seat purchases = %v", seats.calls)
	}

	// Require SSO: review lists alice (no SSO sign-in), the owner is exempt by definition.
	on := true
	_, err = s.UpdateSSO(ctx, owner, team, SSOSettings{Required: &on})
	var review *ReviewRequired
	if !errors.As(err, &review) || len(review.Members) != 1 || review.Members[0].AccountID != alice {
		t.Fatalf("review err=%v", err)
	}
	if _, err = s.UpdateSSO(ctx, owner, team, SSOSettings{Required: &on, Confirm: review.Digest}); err != nil {
		t.Fatal(err)
	}
	guard := func(account string) error {
		return tx(func(ctx context.Context, tx *db.Tx) error { return teams.SSOGuardTx(ctx, tx, team, account) })
	}
	if err = guard(alice); !errors.Is(err, teams.ErrSSORequired) {
		t.Fatalf("alice without SSO: %v", err)
	}
	if err = guard(owner); err != nil {
		t.Fatalf("owner break-glass: %v", err)
	}
	if err = tx(func(ctx context.Context, tx *db.Tx) error {
		return teams.RecordSSOSignInTx(ctx, tx, "org_test", alice, "alice@"+domain)
	}); err != nil {
		t.Fatal(err)
	}
	if err = guard(alice); err != nil {
		t.Fatalf("alice after SSO: %v", err)
	}
	var bypass int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE resource_id=$1 AND event_type='team.sso_bypassed' AND actor_user_id=$2`, team, owner).Scan(&bypass); err != nil || bypass != 1 {
		t.Fatalf("owner bypass audited %d times err=%v", bypass, err)
	}

	// Bob signs up: the held directory invitation becomes membership.
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status,email_verified) VALUES($1,$1,'bob@'||$2,'active',true)`, bob, domain)
	if err = tx(func(ctx context.Context, tx *db.Tx) error {
		return teams.AcceptDirectoryInvitationsTx(ctx, tx, bob, "bob@"+domain)
	}); err != nil {
		t.Fatal(err)
	}
	var bobActive bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT active FROM paperboat.team_members WHERE team_id=$1 AND account_id=$2`, team, bob).Scan(&bobActive); err != nil || !bobActive {
		t.Fatalf("bob member=%v err=%v", bobActive, err)
	}

	// Deprovisioning: alice suspended in the directory, the owner removed from it.
	fake.mu.Lock()
	fake.state.Users = []DirectoryUser{{ID: "du_alice", Email: "alice@" + domain, Active: false}, {ID: "du_bob", Email: "bob@" + domain, Active: true}}
	fake.mu.Unlock()
	if err = s.Reconcile(ctx, team); err != nil {
		t.Fatal(err)
	}
	var aliceActive, ownerActive bool
	_ = store.SQL().QueryRowContext(ctx, `SELECT active FROM paperboat.team_members WHERE team_id=$1 AND account_id=$2`, team, alice).Scan(&aliceActive)
	_ = store.SQL().QueryRowContext(ctx, `SELECT active FROM paperboat.team_members WHERE team_id=$1 AND account_id=$2`, team, owner).Scan(&ownerActive)
	if aliceActive || !ownerActive {
		t.Fatalf("after deprovisioning alice=%v owner=%v", aliceActive, ownerActive)
	}

	// JIT: dave signs in through the team's SSO and becomes a member.
	jit := true
	if _, err = s.UpdateSSO(ctx, owner, team, SSOSettings{JIT: &jit}); err != nil {
		t.Fatal(err)
	}
	if err = tx(func(ctx context.Context, tx *db.Tx) error {
		return teams.RecordSSOSignInTx(ctx, tx, "org_test", dave, "dave@"+domain)
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.processJIT(ctx); err != nil {
		t.Fatal(err)
	}
	if err = guard(dave); err != nil {
		t.Fatalf("dave after JIT: %v", err)
	}
	var events int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE resource_id=$1 AND event_type IN ('team.directory_provisioned','team.directory_invited','team.directory_deprovisioned','team.directory_owner_kept','team.jit_provisioned','team.sso_signed_in')`, team).Scan(&events); err != nil || events < 6 {
		t.Fatalf("audit events=%d err=%v", events, err)
	}
}
