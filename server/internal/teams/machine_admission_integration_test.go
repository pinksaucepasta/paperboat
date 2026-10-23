package teams

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A team that asks for approval holds machines members add until an owner or admin accepts them;
// owners and admins add directly, and the setting can be switched off.
func TestPostgresMachineAdmission(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	owner, member2, outsider, team := "ma_owner_"+suffix, "ma_member_"+suffix, "ma_out_"+suffix, "ma-team-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	defer func() {
		clean, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		for _, q := range []string{`ALTER TABLE paperboat.audit_events DISABLE TRIGGER audit_events_append_only`, `DELETE FROM paperboat.audit_events WHERE actor_user_id IN ('` + owner + `','` + member2 + `')`, `DELETE FROM paperboat.user_machines WHERE user_id IN ('` + owner + `','` + member2 + `')`, `DELETE FROM paperboat.control_environments WHERE owner_user_id IN ('` + owner + `','` + member2 + `')`, `DELETE FROM paperboat.billing_workspaces WHERE team_id='` + team + `'`, `DELETE FROM paperboat.teams WHERE team_id='` + team + `'`, `DELETE FROM paperboat.users WHERE id IN ('` + owner + `','` + member2 + `','` + outsider + `')`, `ALTER TABLE paperboat.audit_events ENABLE TRIGGER audit_events_append_only`} {
			if _, e := store.SQL().ExecContext(clean, q); e != nil {
				t.Error(e)
			}
		}
	}()
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT x,x,x||'@invalid.test','active' FROM unnest($1::text[]) x`, []string{owner, member2, outsider})
	s := NewService(store)
	state, err := s.Create(ctx, owner, CreateRequest{OperationID: "create", TeamID: team})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',3,$3,$4)`, "bw_"+team, team, now.Add(-time.Hour), now.Add(time.Hour))
	state, _ = s.Get(ctx, owner, team)
	invitation, err := s.Invite(ctx, owner, team, InviteRequest{OperationID: "invite", ExpectedGeneration: state.Generation, AccountID: member2})
	if err != nil {
		t.Fatal(err)
	}
	if state, err = s.Accept(ctx, member2, invitation.InvitationID, AcceptRequest{OperationID: "accept"}); err != nil {
		t.Fatal(err)
	}
	if !state.ApproveNewMachines {
		t.Fatal("approval must be on by default")
	}
	machine := func(id, user string) string {
		t.Helper()
		id += "_" + suffix
		exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, "env_"+id, user)
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'linux','amd64','/workspace','offline','released')`, id, user, "env_"+id)
		return id
	}
	op := 0
	act := func(account string, request MachineRequest) (Team, error) {
		t.Helper()
		op++
		current, err := s.Get(ctx, account, team)
		if err != nil {
			t.Fatal(err)
		}
		request.OperationID, request.ExpectedGeneration = "op"+string(rune('a'+op)), current.Generation
		return s.Machine(ctx, account, team, request)
	}
	setApproval := func(on bool) {
		t.Helper()
		op++
		current, _ := s.Get(ctx, owner, team)
		if _, err := s.Mutate(ctx, owner, team, MutationRequest{OperationID: "setting" + string(rune('a'+op)), ExpectedGeneration: current.Generation, Action: "machine_approval", ApproveNewMachines: &on}); err != nil {
			t.Fatal(err)
		}
	}
	bound := func(id string) bool {
		var n int
		if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.team_resource_bindings WHERE team_id=$1 AND resource_kind='machine' AND resource_id=$2 AND active`, team, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}

	// A member's machine waits; only the member and the admins see the request.
	m1 := machine("ma_m1", member2)
	state, err = act(member2, MachineRequest{MachineID: m1, Action: "share"})
	if err != nil {
		t.Fatal(err)
	}
	if bound(m1) || len(state.MachineRequests) != 1 || state.MachineRequests[0].State != "pending" || state.MachineRequests[0].RequestedBy != member2 {
		t.Fatalf("a member's machine was not held for approval: bound=%v requests=%+v", bound(m1), state.MachineRequests)
	}
	if len(ViewFor(state, outsider).MachineRequests) != 0 || len(ViewFor(state, owner).MachineRequests) != 1 || len(ViewFor(state, member2).MachineRequests) != 1 {
		t.Fatal("machine requests are visible to the wrong people")
	}
	if _, err = act(member2, MachineRequest{MachineID: m1, Action: "approve"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member approved their own machine: %v", err)
	}
	// An owner accepts it.
	if state, err = act(owner, MachineRequest{MachineID: m1, Action: "approve"}); err != nil {
		t.Fatal(err)
	}
	if !bound(m1) || len(state.MachineRequests) != 0 {
		t.Fatal("approval did not share the machine")
	}

	// Owners and admins add directly.
	m2 := machine("ma_m2", owner)
	if _, err = act(owner, MachineRequest{MachineID: m2, Action: "share"}); err != nil || !bound(m2) {
		t.Fatalf("an owner's machine waited: %v", err)
	}

	// A refusal is kept for the member, who can ask again or withdraw.
	m3 := machine("ma_m3", member2)
	if _, err = act(member2, MachineRequest{MachineID: m3, Action: "share"}); err != nil {
		t.Fatal(err)
	}
	if state, err = act(owner, MachineRequest{MachineID: m3, Action: "decline"}); err != nil {
		t.Fatal(err)
	}
	if bound(m3) || len(state.MachineRequests) != 1 || state.MachineRequests[0].State != "declined" || state.MachineRequests[0].DecidedBy != owner {
		t.Fatalf("decline not recorded: %+v", state.MachineRequests)
	}
	if state, err = act(member2, MachineRequest{MachineID: m3, Action: "share"}); err != nil || state.MachineRequests[0].State != "pending" {
		t.Fatalf("asking again did not reopen the request: %v", err)
	}
	if state, err = act(member2, MachineRequest{MachineID: m3, Action: "unshare"}); err != nil || len(state.MachineRequests) != 0 {
		t.Fatalf("withdrawing left the request: %v", err)
	}

	// Switching approval off admits members' machines at once.
	setApproval(false)
	m4 := machine("ma_m4", member2)
	if _, err = act(member2, MachineRequest{MachineID: m4, Action: "share"}); err != nil || !bound(m4) {
		t.Fatalf("a machine waited with approval off: %v", err)
	}
	setApproval(true)

	// The enrollment path follows the same rule.
	m5, m6 := machine("ma_m5", member2), machine("ma_m6", owner)
	for _, c := range []struct{ user, id, want string }{{member2, m5, AdmissionPending}, {owner, m6, AdmissionAccepted}} {
		var got string
		if err = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			var e error
			got, e = ShareEnrolledMachineTx(ctx, tx, c.user, team, c.id)
			return e
		}); err != nil || got != c.want {
			t.Fatalf("enrollment into the team for %s: %q %v, want %q", c.user, got, err, c.want)
		}
	}
	if bound(m5) || !bound(m6) {
		t.Fatal("enrolled machines were placed against the rule")
	}
	if got, err := s.AdmissionFor(ctx, member2, team); err != nil || got != AdmissionPending {
		t.Fatalf("admission for a member with approval on: %q %v", got, err)
	}
}
