package teams

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/policy"
)

type policyFixture struct {
	t                           *testing.T
	ctx                         context.Context
	store                       *db.DB
	service                     *Service
	team                        string
	owner, admin, member, other string
	db1, web1, web1b, runner    string
}

// newPolicyFixture: a paid team; owner, admin, member, other; db1 and runner are team
// machines, web1 is member's personal machine shared into the team and web1b other's
// personal machine with the same alias.
func newPolicyFixture(t *testing.T) *policyFixture {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
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
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	f := &policyFixture{t: t, ctx: ctx, store: store, service: NewService(store), team: "pol-" + s,
		owner: "po_" + s, admin: "pa_" + s, member: "pm_" + s, other: "px_" + s,
		db1: "mdb_" + s, web1: "mweb_" + s, web1b: "mwebb_" + s, runner: "mrun_" + s}
	f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT u,u,u||'@example.test','active' FROM unnest($1::text[]) u`, []string{f.owner, f.admin, f.member, f.other})
	f.exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, f.team, f.owner)
	f.exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',4,now()-interval '1 hour',now()+interval '1 hour')`, "bw-"+f.team, f.team)
	f.exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'admin',true),($1,$4,1,'member',true),($1,$5,1,'member',true)`, f.team, f.owner, f.admin, f.member, f.other)
	for _, m := range [][4]string{{f.db1, "db1", f.owner, f.team}, {f.runner, "runner", f.admin, f.team}, {f.web1, "web1", f.member, ""}, {f.web1b, "web1", f.other, ""}} {
		f.exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,owner_team_id) VALUES($1,$2,$1,$3,'linux','amd64','/w','online','occupied',true,nullif($4,''))`, m[0], m[2], m[1], m[3])
		f.exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'machine',$2,$3)`, f.team, m[0], m[2])
	}
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.team_resource_bindings WHERE team_id=$1`, f.team)
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.user_machines WHERE id=ANY($1)`, []string{f.db1, f.web1, f.web1b, f.runner})
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.teams WHERE team_id=$1`, f.team)
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.api_tokens WHERE account_id=ANY($1)`, []string{f.owner, f.admin, f.member, f.other})
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.users WHERE id=ANY($1)`, []string{f.owner, f.admin, f.member, f.other})
	})
	return f
}

func (f *policyFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(f.ctx, q, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *policyFixture) email(account string) string { return account + "@example.test" }

// save writes source as owner, confirming whatever access change it makes.
func (f *policyFixture) save(source string) PolicyView {
	f.t.Helper()
	cur, err := f.service.Policy(f.ctx, f.owner, f.team, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	req := PolicySaveRequest{Policy: source, ExpectedVersion: cur.Version}
	out, err := f.service.SavePolicy(f.ctx, f.owner, f.team, req)
	var unconfirmed *PolicyUnconfirmedError
	if errors.As(err, &unconfirmed) {
		req.Confirm = unconfirmed.Confirm
		out, err = f.service.SavePolicy(f.ctx, f.owner, f.team, req)
	}
	if err != nil {
		f.t.Fatalf("save: %v\n%s", err, source)
	}
	return out
}

func (f *policyFixture) sqlAllowed(actor, machine, capability string, port int) bool {
	f.t.Helper()
	var ok bool
	var err error
	if port == 0 {
		err = f.store.SQL().QueryRowContext(f.ctx, `SELECT paperboat.team_machine_capability_allowed($1,$2,$3,$4)`, actor, f.team, machine, capability).Scan(&ok)
	} else {
		err = f.store.SQL().QueryRowContext(f.ctx, `SELECT paperboat.team_machine_port_allowed($1,$2,$3,$4)`, actor, f.team, machine, port).Scan(&ok)
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return ok
}

// The SQL check that enforces access and shared/policy Decide that explains it must agree.
func TestPolicySQLMatchesGo(t *testing.T) {
	f := newPolicyFixture(t)
	m, o, a := "user:"+f.email(f.member), "user:"+f.email(f.other), "user:"+f.email(f.admin)
	policies := []string{
		`{"rules": []}`,
		fmt.Sprintf(`{"rules": [{"src": ["role:member"], "dst": ["machine:db1"], "allow": ["terminal", "exec"]}, {"src": ["%s"], "dst": ["machine:%s/web1"], "allow": ["*"]}]}`, o, f.email(f.member)),
		fmt.Sprintf(`{"allow_private_access": true, "groups": {"group:ops": ["%s", "%s"]}, "tags": {"tag:prod": ["machine:db1", "machine:runner"]},
		  "rules": [
		    {"src": ["group:ops"], "dst": ["tag:prod"], "allow": ["*"]},
		    {"src": ["*"], "dst": ["tag:prod"], "deny": ["managed_ssh"]},
		    {"src": ["role:admin"], "dst": ["machine:*"], "allow": ["private_access"], "ports": [22, "5000-6000"]},
		    {"src": ["%s"], "dst": ["machine:db1"], "deny": ["private_access"], "ports": [5432]},
		    {"src": ["role:owner"], "dst": ["machine:%s/web1"], "allow": ["files"]}
		  ]}`, m, a, m, f.email(f.other)),
		fmt.Sprintf(`{"allow_private_access": false, "rules": [{"src": ["*"], "dst": ["machine:*"], "allow": ["private_access", "tunnel_manage"]}, {"src": ["%s"], "dst": ["machine:runner"], "deny": ["*"]}]}`, m),
	}
	members := []string{f.owner, f.admin, f.member, f.other}
	machines := []string{f.db1, f.runner, f.web1, f.web1b}
	for i, source := range policies {
		f.save(source)
		var inv policy.Inventory
		if err := f.store.InTx(f.ctx, func(ctx context.Context, tx *db.Tx) (err error) {
			inv, err = policyInventoryTx(ctx, tx, Workspace{TeamID: f.team})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		_, c, errs := policy.Validate([]byte(source), inv, policy.Limits{})
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		// The audience of a machine's topic wakes (one set-based query) is exactly the members with any capability.
		for _, machine := range machines {
			var audience, want string
			if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT
 coalesce((SELECT string_agg(a, ',' ORDER BY a) FROM paperboat.workspace_policy_audience($1, $2) a), ''),
 coalesce((SELECT string_agg(m.account_id, ',' ORDER BY m.account_id) FROM paperboat.team_members m WHERE m.team_id=$1 AND m.active
  AND EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage','private_access']) cap
   WHERE NOT isempty(paperboat.workspace_policy_ports(m.account_id, $1, $2, cap)))), '')`, f.team, machine).Scan(&audience, &want); err != nil {
				t.Fatal(err)
			}
			if audience != want {
				t.Errorf("policy %d: %s audience %q, members with a capability %q", i, machine, audience, want)
			}
		}
		for _, actor := range members {
			for _, machine := range machines {
				target := policy.Target{Kind: "machine", ID: machine}
				for _, capability := range policy.MachineCapabilities {
					want := c.Decide(actor, target, capability, 0).Allowed
					if got := f.sqlAllowed(actor, machine, capability, 0); got != want {
						t.Errorf("policy %d: %s %s %s: SQL %v, Go %v", i, actor, machine, capability, got, want)
					}
				}
				for _, port := range []int{22, 1023, 1024, 5432, 5500, 65535} {
					want := c.Decide(actor, target, policy.ActionPrivateAccess, port).Allowed
					if got := f.sqlAllowed(actor, machine, "", port); got != want {
						t.Errorf("policy %d: %s %s port %d: SQL %v, Go %v", i, actor, machine, port, got, want)
					}
				}
			}
		}
	}
}

func TestPolicySaveReviewAndRights(t *testing.T) {
	f := newPolicyFixture(t)
	cur, err := f.service.Policy(f.ctx, f.owner, f.team, 0)
	if err != nil || cur.CanEdit != "all" || cur.Version != 1 {
		t.Fatalf("%+v %v", cur, err)
	}
	// A team's version 1 lets owners and admins use the team ENV; this save keeps that rule.
	gain := `{"rules": [{"src": ["role:admin"], "dst": ["env:team"], "allow": ["env_read", "env_write"]}, {"src": ["role:member"], "dst": ["machine:db1"], "allow": ["exec"]}]}`
	_, err = f.service.SavePolicy(f.ctx, f.owner, f.team, PolicySaveRequest{Policy: `{"rules": [{"src": ["*"], "dst": ["machine:nope"], "allow": ["exec"]}]}`, ExpectedVersion: 1})
	var invalid *PolicyInvalidError
	if !errors.As(err, &invalid) || invalid.Errors[0].Code != "unknown_machine" {
		t.Fatalf("invalid file saved: %v", err)
	}
	_, err = f.service.SavePolicy(f.ctx, f.owner, f.team, PolicySaveRequest{Policy: gain, ExpectedVersion: 1})
	var unconfirmed *PolicyUnconfirmedError
	if !errors.As(err, &unconfirmed) || len(unconfirmed.Changes) != 4 {
		t.Fatalf("a save that gives four members exec must be confirmed: %v", err)
	}
	saved, err := f.service.SavePolicy(f.ctx, f.owner, f.team, PolicySaveRequest{Policy: gain, ExpectedVersion: 1, Confirm: unconfirmed.Confirm, Message: "exec for all"})
	if err != nil || saved.Version != 2 || !f.sqlAllowed(f.other, f.db1, "exec", 0) {
		t.Fatalf("%+v %v", saved, err)
	}
	if _, err = f.service.SavePolicy(f.ctx, f.owner, f.team, PolicySaveRequest{Policy: `{"rules": []}`, ExpectedVersion: 1}); !errors.Is(err, ErrPolicyConflict) {
		t.Fatalf("stale save: %v", err)
	}
	if _, err = f.service.SavePolicy(f.ctx, f.other, f.team, PolicySaveRequest{Policy: `{"rules": []}`, ExpectedVersion: 2}); !errors.Is(err, ErrPolicyForbidden) {
		t.Fatalf("a member without shared machines cannot edit: %v", err)
	}
	// member owns web1 (shared): may change rules for it only.
	view, _ := f.service.Policy(f.ctx, f.member, f.team, 0)
	if view.CanEdit != "own_machines" {
		t.Fatalf("%+v", view)
	}
	own := fmt.Sprintf(`{"rules": [{"src": ["role:admin"], "dst": ["env:team"], "allow": ["env_read", "env_write"]}, {"src": ["role:member"], "dst": ["machine:db1"], "allow": ["exec"]}, {"src": ["role:member"], "dst": ["machine:%s/web1"], "allow": ["files"]}]}`, f.email(f.member))
	if _, err = f.service.SavePolicy(f.ctx, f.member, f.team, PolicySaveRequest{Policy: own, ExpectedVersion: 2}); err != nil && !errors.As(err, &unconfirmed) {
		t.Fatalf("own machine edit refused: %v", err)
	}
	if _, err = f.service.SavePolicy(f.ctx, f.member, f.team, PolicySaveRequest{Policy: `{"rules": []}`, ExpectedVersion: 2}); !errors.Is(err, ErrPolicyForbidden) {
		t.Fatalf("a member removed a rule for db1: %v", err)
	}
	// Explain: members about themselves only.
	if _, err = f.service.ExplainPolicy(f.ctx, f.member, f.team, PolicyExplainRequest{Src: "user:" + f.email(f.other), Dst: "machine:db1", Action: "exec"}); !errors.Is(err, ErrPolicyExplainForbidden) {
		t.Fatalf("%v", err)
	}
	ex, err := f.service.ExplainPolicy(f.ctx, f.member, f.team, PolicyExplainRequest{Src: "user:" + f.email(f.member), Dst: "machine:db1", Action: "exec"})
	if err != nil || ex.Decision == nil || !ex.Decision.Allowed || ex.Decision.Rules[0] != "/rules/1" {
		t.Fatalf("%+v %v", ex.Decision, err)
	}
	// A scoped API token is narrowed by its scope even where its creator is allowed.
	f.exec(`INSERT INTO paperboat.api_tokens(id,account_id,team_id,name,secret_hash,scope,expires_at) VALUES($1,$2,$3,'ci',sha256(convert_to($1::text,'UTF8')),'[{"dst":["machine:runner"],"allow":["exec"]}]',now()+interval '1 day')`, "tok_"+f.member, f.member, f.team)
	ex, err = f.service.ExplainPolicy(f.ctx, f.member, f.team, PolicyExplainRequest{Src: "user:" + f.email(f.member), Dst: "machine:db1", Action: "exec", APIToken: "ci"})
	if err != nil || ex.Decision == nil || ex.Decision.Allowed || ex.Decision.Reason != "outside_token_scope" {
		t.Fatalf("scoped token: %+v %v", ex.Decision, err)
	}
	if _, err = f.service.ExplainPolicy(f.ctx, f.member, f.team, PolicyExplainRequest{Src: "user:" + f.email(f.member), Dst: "machine:db1", Action: "exec", APIToken: "nope"}); !errors.As(err, &invalid) {
		t.Fatalf("unknown token: %v", err)
	}
	// Restore version 1 (no rules) as version 3.
	_, err = f.service.RestorePolicy(f.ctx, f.owner, f.team, PolicyRestoreRequest{Version: 1, ExpectedVersion: 2})
	if !errors.As(err, &unconfirmed) {
		t.Fatalf("restore that removes access must be confirmed: %v", err)
	}
	restored, err := f.service.RestorePolicy(f.ctx, f.owner, f.team, PolicyRestoreRequest{Version: 1, ExpectedVersion: 2, Confirm: unconfirmed.Confirm})
	if err != nil || restored.Version != 3 || f.sqlAllowed(f.other, f.db1, "exec", 0) {
		t.Fatalf("%+v %v", restored, err)
	}
	versions, err := f.service.PolicyVersions(f.ctx, f.admin, f.team)
	if err != nil || len(versions) != 3 || versions[0].Kind != "restored" || versions[1].Message != "exec for all" || versions[1].Gained != 4 || versions[0].Lost != 4 {
		t.Fatalf("%+v %v", versions, err)
	}
}

// The simple view edits the policy file; departures and unsharing drop references for good.
func TestPolicySimpleViewAndDepartures(t *testing.T) {
	f := newPolicyFixture(t)
	grant := func(account string, caps ...string) {
		t.Helper()
		r := MachineGrantRequest{MachineID: f.db1, Audience: "selected_member", AccountID: account, Capabilities: caps, Active: len(caps) > 0}
		if err := f.store.InTx(f.ctx, func(ctx context.Context, tx *db.Tx) error {
			if err := Lock(ctx, tx); err != nil {
				return err
			}
			return ApplyMachineAccessTx(ctx, tx, config.Default().WorkspacePolicy, f.team, f.owner, r)
		}); err != nil {
			t.Fatal(err)
		}
	}
	grant(f.member, "terminal", "exec")
	view, _ := f.service.Policy(f.ctx, f.owner, f.team, 0)
	if !strings.Contains(view.Policy, `"user:`+f.email(f.member)+`"`) || !strings.Contains(view.Policy, `"machine:db1"`) {
		t.Fatalf("the file shows names:\n%s", view.Policy)
	}
	var stored string
	f.store.SQL().QueryRowContext(f.ctx, `SELECT source FROM paperboat.workspace_policies WHERE team_id=$1`, f.team).Scan(&stored)
	if !strings.Contains(stored, `"user:#`+f.member+`"`) || !strings.Contains(stored, `"machine:#`+f.db1+`"`) {
		t.Fatalf("the stored file uses IDs:\n%s", stored)
	}
	var grants []MachineGrant
	f.store.InTx(f.ctx, func(ctx context.Context, tx *db.Tx) (err error) {
		p, err := teamPolicyTx(ctx, tx, f.team)
		grants = machineGrants(p)
		return err
	})
	if len(grants) != 1 || grants[0].AccountID != f.member || strings.Join(grants[0].Capabilities, ",") != "exec,terminal" {
		t.Fatalf("%+v", grants)
	}
	if !f.sqlAllowed(f.member, f.db1, "exec", 0) {
		t.Fatal("simple view grant not enforced")
	}
	// The member leaves and comes back: no access is inherited.
	if err := f.store.InTx(f.ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := Lock(ctx, tx); err != nil {
			return err
		}
		return RemoveMemberTx(ctx, tx, f.team, f.member)
	}); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE paperboat.team_members SET active=true WHERE team_id=$1 AND account_id=$2`, f.team, f.member)
	if f.sqlAllowed(f.member, f.db1, "exec", 0) {
		t.Fatal("a member who left and rejoined kept access")
	}
	f.store.SQL().QueryRowContext(f.ctx, `SELECT source FROM paperboat.workspace_policies WHERE team_id=$1`, f.team).Scan(&stored)
	if strings.Contains(stored, f.member) {
		t.Fatalf("departed member still referenced:\n%s", stored)
	}
	// Renaming a machine changes only how the file reads.
	f.exec(`UPDATE paperboat.user_machines SET alias='db9' WHERE id=$1`, f.db1)
	grant(f.other, "files")
	view, _ = f.service.Policy(f.ctx, f.owner, f.team, 0)
	if !strings.Contains(view.Policy, `"machine:db9"`) || !f.sqlAllowed(f.other, f.db1, "files", 0) {
		t.Fatalf("%s", view.Policy)
	}
	// Private access switch through the simple view.
	if err := f.store.InTx(f.ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := Lock(ctx, tx); err != nil {
			return err
		}
		return SetAllowPrivateAccessTx(ctx, tx, config.Default().WorkspacePolicy, f.team, f.owner, true)
	}); err != nil {
		t.Fatal(err)
	}
	var on bool
	f.store.SQL().QueryRowContext(f.ctx, `SELECT allow_private_access FROM paperboat.workspace_policies WHERE team_id=$1`, f.team).Scan(&on)
	view, _ = f.service.Policy(f.ctx, f.owner, f.team, 0)
	if !on || !strings.Contains(view.Policy, `"allow_private_access": true`) {
		t.Fatalf("%v\n%s", on, view.Policy)
	}
}

// F18-07: the team ENV follows the policy. A team starts with owners and admins on it; an ENV
// grant is a member's rule; explain says when a key has not been delivered yet; a save that
// removes a key holder's read takes their key and requires a rotation, as pb env team revoke.
func TestPolicyDecidesTeamENV(t *testing.T) {
	f := newPolicyFixture(t)
	f.exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'env',$1,$2)`, f.team, f.owner)
	f.exec(`INSERT INTO paperboat.environment_vault_teams(team_id,key_epoch) VALUES($1,1)`, f.team)
	f.exec(`INSERT INTO paperboat.environment_vault_team_members(team_id,account_id,grant_epoch) VALUES($1,$2,1)`, f.team, f.owner)
	if _, err := f.service.Policy(f.ctx, f.owner, f.team, 0); err != nil {
		t.Fatal(err)
	}
	permission := func(account string) (p string) {
		t.Helper()
		if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT paperboat.team_env_permission($1,$2)`, account, f.team).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for account, want := range map[string]string{f.owner: "write", f.admin: "write", f.member: "", f.other: ""} {
		if got := permission(account); got != want {
			t.Fatalf("new team: %s ENV %q, want %q", account, got, want)
		}
	}
	grant := func(account, perm string, active bool) Team {
		t.Helper()
		var generation uint64
		if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT generation FROM paperboat.teams WHERE team_id=$1`, f.team).Scan(&generation); err != nil {
			t.Fatal(err)
		}
		out, err := f.service.Grant(f.ctx, f.owner, f.team, GrantRequest{OperationID: fmt.Sprintf("op_env_%s_%s_%v_%d", account, perm, active, generation), ExpectedGeneration: generation, AccountID: account, ResourceKind: "env", ResourceID: f.team, Permission: perm, Active: active})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	team := grant(f.member, "read", true)
	grant(f.other, "write", true)
	if permission(f.member) != "read" || permission(f.other) != "write" {
		t.Fatalf("grants: member %q other %q", permission(f.member), permission(f.other))
	}
	shown := false
	for _, g := range team.Grants {
		shown = shown || (g.ResourceKind == "env" && g.AccountID == f.member && g.Permission == "read" && g.Active)
	}
	if !shown {
		t.Fatalf("the team view does not show the member's ENV grant: %+v", team.Grants)
	}
	f.exec(`INSERT INTO paperboat.environment_vault_team_members(team_id,account_id,grant_epoch) VALUES($1,$2,1)`, f.team, f.member)
	explain := func(account string) *PolicyDecision {
		t.Helper()
		ex, err := f.service.ExplainPolicy(f.ctx, f.owner, f.team, PolicyExplainRequest{Src: "user:" + f.email(account), Dst: "env:team", Action: "env_read"})
		if err != nil {
			t.Fatal(err)
		}
		return ex.Decision
	}
	if d := explain(f.member); !d.Allowed || d.KeyDelivered != nil {
		t.Fatalf("member holds the key: %+v", d)
	}
	if d := explain(f.other); !d.Allowed || d.KeyDelivered == nil || *d.KeyDelivered {
		t.Fatalf("other is allowed but has no key yet: %+v", d)
	}
	cur, err := f.service.Policy(f.ctx, f.owner, f.team, 0)
	if err != nil {
		t.Fatal(err)
	}
	next, err := policy.SetSimpleRule([]byte(cur.Policy), "user:"+f.email(f.member), "env:team", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.save(string(next))
	var epoch uint64
	var rotation bool
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT e.grant_epoch,t.rotation_required FROM paperboat.environment_vault_team_members e JOIN paperboat.environment_vault_teams t USING(team_id) WHERE e.team_id=$1 AND e.account_id=$2`, f.team, f.member).Scan(&epoch, &rotation); err != nil {
		t.Fatal(err)
	}
	if permission(f.member) != "" || epoch != 0 || !rotation {
		t.Fatalf("removing the member's ENV rule: permission %q, key epoch %d, rotation required %v; want none, 0, true", permission(f.member), epoch, rotation)
	}
	if grant(f.other, "write", false); permission(f.other) != "" || permission(f.admin) != "write" {
		t.Fatalf("revoking other's grant: other %q admin %q", permission(f.other), permission(f.admin))
	}
}

// Preview/tunnel inspect and replay: the SQL check behind inspector credentials and shared/policy
// Decide agree for every member, resource and action, across resource rules, machine rules,
// tags, deny and the owner guardrail; pb team grant ... inspect edits one member's rule.
func TestPolicyInspectReplaySQLMatchesGo(t *testing.T) {
	f := newPolicyFixture(t)
	// api runs on the team machine db1 (owner's); web runs on member's shared web1.
	api, web := "api-"+f.team, "web-"+f.team
	for i, tn := range [][3]string{{api, f.owner, f.db1}, {web, f.member, f.web1}} {
		hex := fmt.Sprintf("%011d%d", time.Now().UnixNano()%1e11, i)
		f.exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,created_at,updated_at) VALUES($1,$2,$1,'active','team',1,$4,$5,$3,$2,now(),now())`,
			tn[0], tn[1], tn[2], "endpoint_00000000-0000-4000-8000-"+hex, "https://00000000-0000-8000-8000-"+hex+".tunnels.example.test")
		f.exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'tunnel',$2,$3)`, f.team, tn[0], tn[1])
	}
	t.Cleanup(func() {
		_, _ = f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.tunnels WHERE id=ANY($1)`, []string{api, web})
	})
	o := "user:" + f.email(f.other)
	policies := []string{
		`{"rules": []}`,
		fmt.Sprintf(`{"rules": [{"src": ["%s"], "dst": ["tunnel:%s"], "allow": ["inspect"]}, {"src": ["role:member"], "dst": ["machine:db1"], "allow": ["replay"]}]}`, o, api),
		fmt.Sprintf(`{"tags": {"tag:prod": ["machine:db1"]}, "rules": [
		  {"src": ["*"], "dst": ["tag:prod"], "allow": ["inspect", "replay"]},
		  {"src": ["%s"], "dst": ["tunnel:%s"], "deny": ["replay"]},
		  {"src": ["*"], "dst": ["machine:*"], "deny": ["inspect"]}]}`, o, api),
	}
	members := []string{f.owner, f.admin, f.member, f.other}
	for i, source := range policies {
		f.save(source)
		var inv policy.Inventory
		if err := f.store.InTx(f.ctx, func(ctx context.Context, tx *db.Tx) (err error) {
			inv, err = policyInventoryTx(ctx, tx, Workspace{TeamID: f.team})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		_, c, errs := policy.Validate([]byte(source), inv, policy.Limits{})
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		for _, actor := range members {
			for _, tn := range []string{api, web} {
				for _, action := range policy.ResourceActions {
					want := c.Decide(actor, policy.Target{Kind: "tunnel", ID: tn}, action, 0).Allowed
					var got bool
					if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT paperboat.workspace_policy_resource_allowed($1,$2,'tunnel',$3,$4)`, actor, f.team, tn, action).Scan(&got); err != nil {
						t.Fatal(err)
					}
					if got != want {
						t.Errorf("policy %d: %s %s %s: SQL %v, Go %v", i, actor, tn, action, got, want)
					}
				}
			}
		}
	}
	// The simple view: pb team grant adds and removes one action of one member's rule.
	f.save(`{"rules": []}`)
	grant := func(permission string, active bool) Team {
		t.Helper()
		var generation uint64
		if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT generation FROM paperboat.teams WHERE team_id=$1`, f.team).Scan(&generation); err != nil {
			t.Fatal(err)
		}
		out, err := f.service.Grant(f.ctx, f.owner, f.team, GrantRequest{OperationID: fmt.Sprintf("op_ins_%s_%v_%d", permission, active, generation), ExpectedGeneration: generation, AccountID: f.other, ResourceKind: "tunnel", ResourceID: api, Permission: permission, Active: active})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	grant("inspect", true)
	team := grant("replay", true)
	shown := map[string]bool{}
	for _, g := range team.Grants {
		if g.AccountID == f.other && g.ResourceID == api {
			shown[g.Permission] = g.Active
		}
	}
	if !shown["inspect"] || !shown["replay"] {
		t.Fatalf("team view grants for other on api: %v", shown)
	}
	grant("replay", false)
	for action, want := range map[string]bool{"inspect": true, "replay": false} {
		if d, err := f.service.Authorize(f.ctx, f.other, f.team, "tunnel", api, action); (err == nil) != want {
			t.Fatalf("after revoking replay, %s: %+v %v", action, d, err)
		}
	}
}

// Machine tags as their own action: each change is a policy version with the editor's review
// (412 when it changes access), rights (a member tags only machines they shared), name checks,
// and the workspace's tag list.
func TestPolicyMachineTags(t *testing.T) {
	f := newPolicyFixture(t)
	f.save(`{"rules": [{"src": ["role:admin"], "dst": ["env:team"], "allow": ["env_read", "env_write"]}, {"src": ["role:member"], "dst": ["tag:prod"], "allow": ["exec"]}], "tags": {"tag:prod": []}}`)
	cur, err := f.service.Policy(f.ctx, f.owner, f.team, 0)
	if err != nil {
		t.Fatal(err)
	}
	req := MachineTagsRequest{Tags: []string{"tag:prod", "tag:new"}, ExpectedVersion: cur.Version}
	_, err = f.service.SetMachineTags(f.ctx, f.owner, f.team, f.db1, req)
	var unconfirmed *PolicyUnconfirmedError
	if !errors.As(err, &unconfirmed) || len(unconfirmed.Changes) == 0 {
		t.Fatalf("tagging db1 tag:prod gives members exec there and must be confirmed: %v", err)
	}
	req.Confirm = unconfirmed.Confirm
	out, err := f.service.SetMachineTags(f.ctx, f.owner, f.team, f.db1, req)
	if err != nil || out.PolicyVersion != cur.Version+1 || strings.Join(out.Tags, ",") != "tag:new,tag:prod" || !f.sqlAllowed(f.other, f.db1, "exec", 0) {
		t.Fatalf("%+v %v", out, err)
	}
	list, err := f.service.WorkspaceTagList(f.ctx, f.member, f.team)
	if err != nil || list.CanEdit != "own_machines" || len(list.Tags) != 2 || list.Tags[1].Name != "tag:prod" || strings.Join(list.Tags[1].Machines, ",") != f.db1 {
		t.Fatalf("tag list: %+v %v", list, err)
	}
	if _, err = f.service.SetMachineTags(f.ctx, f.owner, f.team, f.db1, MachineTagsRequest{Tags: []string{"tag:new"}, ExpectedVersion: cur.Version}); !errors.Is(err, ErrPolicyConflict) {
		t.Fatalf("stale tag change: %v", err)
	}
	// member shared web1: may tag it, not db1.
	v := out.PolicyVersion
	if _, err = f.service.SetMachineTags(f.ctx, f.member, f.team, f.db1, MachineTagsRequest{Tags: nil, ExpectedVersion: v}); !errors.Is(err, ErrPolicyForbidden) {
		t.Fatalf("a member untagged someone else's machine: %v", err)
	}
	if out, err = f.service.SetMachineTags(f.ctx, f.member, f.team, f.web1, MachineTagsRequest{Tags: []string{"tag:mine"}, ExpectedVersion: v}); err != nil || strings.Join(out.Tags, ",") != "tag:mine" {
		t.Fatalf("a member tags their own shared machine: %+v %v", out, err)
	}
	var invalid *PolicyInvalidError
	if _, err = f.service.SetMachineTags(f.ctx, f.owner, f.team, f.db1, MachineTagsRequest{Tags: []string{"Prod"}, ExpectedVersion: out.PolicyVersion}); !errors.As(err, &invalid) || invalid.Errors[0].Path != "/tags/0" || invalid.Errors[0].Code != "invalid_name" {
		t.Fatalf("invalid tag name: %v", err)
	}
	if _, err = f.service.SetMachineTags(f.ctx, f.owner, f.team, "machine_not_here", MachineTagsRequest{ExpectedVersion: out.PolicyVersion}); !errors.Is(err, ErrPolicyMachineNotFound) {
		t.Fatalf("unknown machine: %v", err)
	}
	// Untagging db1 from everything keeps tag:prod (a rule names it) and drops tag:new.
	if out, err = f.service.SetMachineTags(f.ctx, f.owner, f.team, f.db1, MachineTagsRequest{ExpectedVersion: out.PolicyVersion, Confirm: ""}); err != nil {
		if errors.As(err, &unconfirmed) {
			out, err = f.service.SetMachineTags(f.ctx, f.owner, f.team, f.db1, MachineTagsRequest{ExpectedVersion: v + 1, Confirm: unconfirmed.Confirm})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if list, _ = f.service.WorkspaceTagList(f.ctx, f.owner, f.team); len(list.Tags) != 2 || list.Tags[0].Name != "tag:mine" || list.Tags[1].Name != "tag:prod" || len(list.Tags[1].Machines) != 0 {
		t.Fatalf("after untagging db1: %+v", list.Tags)
	}
}

// A save that changes a machine's tags tells every machine that lists it to refresh its
// inventory, so pb status and the daemons' tags follow at once (F18-11 on device: a personal save
// published nothing, and a team save only machine_access).
func TestPolicyTagChangeRefreshesInventory(t *testing.T) {
	f := newPolicyFixture(t)
	generation := func(machine string) (g int64) {
		t.Helper()
		if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT coalesce((SELECT generation FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic='inventory'),0)`, machine).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	cur, err := f.service.Policy(f.ctx, f.owner, f.team, 0)
	if err != nil {
		t.Fatal(err)
	}
	before := generation(f.db1)
	if _, err = f.service.SetMachineTags(f.ctx, f.owner, f.team, f.db1, MachineTagsRequest{Tags: []string{"tag:ci"}, ExpectedVersion: cur.Version}); err != nil {
		t.Fatal(err)
	}
	if after := generation(f.db1); after <= before {
		t.Fatalf("tagging db1 left its inventory generation at %d->%d", before, after)
	}
	runner := generation(f.runner)
	cur, _ = f.service.Policy(f.ctx, f.owner, f.team, 0)
	f.save(cur.Policy) // a save with no tag change
	if generation(f.runner) != runner {
		t.Fatal("a save that changes no tags must not refresh inventories")
	}
}
