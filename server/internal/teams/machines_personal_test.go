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

// TestPostgresTransferToPersonal: the way back from transfer_to_team. The machine keeps its id and
// history, goes back to the account that enrolled it, and the team's access to it ends.
func TestPostgresTransferToPersonal(t *testing.T) {
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
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
	owner, enroller, plain, gone := "tp_owner_"+suffix, "tp_enroller_"+suffix, "tp_plain_"+suffix, "tp_gone_"+suffix
	team, machine, orphan, clash := "tp-team-"+suffix, "tp_machine_"+suffix, "tp_orphan_"+suffix, "tp_clash_"+suffix
	accounts, machineIDs := []string{owner, enroller, plain, gone}, []string{machine, orphan, clash}
	defer func() {
		c, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		tx, e := store.SQL().BeginTx(c, nil)
		if e != nil {
			t.Error(e)
			return
		}
		defer tx.Rollback()
		for _, q := range []string{
			`ALTER TABLE paperboat.audit_events DISABLE TRIGGER audit_events_append_only`,
			`DELETE FROM paperboat.audit_events WHERE resource_type='team' AND resource_id='` + team + `'`,
			`ALTER TABLE paperboat.audit_events ENABLE TRIGGER audit_events_append_only`,
		} {
			if _, e = tx.ExecContext(c, q); e != nil {
				t.Error(e)
				return
			}
		}
		for _, q := range []string{`DELETE FROM paperboat.user_machines WHERE id=ANY($1::text[])`, `DELETE FROM paperboat.billing_workspaces WHERE team_id=ANY($1::text[])`, `DELETE FROM paperboat.teams WHERE team_id=ANY($1::text[])`, `DELETE FROM paperboat.users WHERE id=ANY($1::text[])`} {
			arg := machineIDs
			if strings.Contains(q, "team_id") {
				arg = []string{team}
			} else if strings.Contains(q, "users") {
				arg = accounts
			}
			if _, e = tx.ExecContext(c, q, arg); e != nil {
				t.Error(e)
				return
			}
		}
		if e = tx.Commit(); e != nil {
			t.Error(e)
		}
	}()
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT x,x,x||'@invalid.test','active' FROM unnest($1::text[]) x`, accounts); err != nil {
		t.Fatal(err)
	}
	s := NewService(store)
	current, err := s.Create(ctx, owner, CreateRequest{OperationID: "create", TeamID: team})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',4,$3,$4)`, "bw_"+team, team, now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{enroller, plain, gone} {
		current, err = s.Get(ctx, owner, team)
		if err != nil {
			t.Fatal(err)
		}
		invitation, e := s.Invite(ctx, owner, team, InviteRequest{OperationID: "invite_" + account, ExpectedGeneration: current.Generation, AccountID: account})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Accept(ctx, account, invitation.InvitationID, AcceptRequest{OperationID: "accept"}); e != nil {
			t.Fatal(e)
		}
	}
	for id, enrolledBy := range map[string]string{machine: enroller, orphan: gone} {
		if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,installation_generation) VALUES($1,$2,$3,'laptop','linux','amd64','/workspace','offline','released',7)`, id, enrolledBy, "env_"+id); err != nil {
			t.Fatal(err)
		}
	}
	act := func(account, id, action, op string) error {
		t.Helper()
		got, e := s.Get(ctx, owner, team)
		if e != nil {
			t.Fatal(e)
		}
		current = got
		_, e = s.Machine(ctx, account, team, MachineRequest{OperationID: op, ExpectedGeneration: current.Generation, MachineID: id, Action: action, Confirmation: id})
		return e
	}
	mutate := func(account string, r MutationRequest) {
		t.Helper()
		got, e := s.Get(ctx, owner, team)
		if e != nil {
			t.Fatal(e)
		}
		r.ExpectedGeneration = got.Generation
		if _, e = s.Mutate(ctx, account, team, r); e != nil {
			t.Fatal(e)
		}
	}
	allowed := func(account, selected, id string, want bool) {
		t.Helper()
		_, e := s.Authorize(ctx, account, selected, "machine", id, "terminal")
		if want && e != nil || !want && !errors.Is(e, ErrForbidden) {
			t.Fatalf("authorize %s/%s want %v: %v", account, selected, want, e)
		}
	}
	mutate(owner, MutationRequest{OperationID: "admin", Action: "role", AccountID: enroller, Role: "admin"})
	mutate(owner, MutationRequest{OperationID: "admin_gone", Action: "role", AccountID: gone, Role: "admin"})
	if err = act(enroller, machine, "transfer_to_team", "to_team"); err != nil {
		t.Fatal(err)
	}
	if err = act(gone, orphan, "transfer_to_team", "to_team_orphan"); err != nil {
		t.Fatal(err)
	}
	// A teammate has terminal access to the team machine through an all-member grant.
	if _, err = s.GrantMachine(ctx, owner, team, MachineGrantRequest{OperationID: "grant_all", ExpectedGeneration: current.Generation, MachineID: machine, Audience: "all_members", Capabilities: []string{"terminal"}, Active: true}); err != nil {
		t.Fatal(err)
	}
	allowed(plain, team, machine, true)
	allowed(enroller, "", machine, false)
	// Terminal sessions opened on the team machine: the enroller's own (shared with the team) and a teammate's.
	mine, theirs := "tp_term_mine_"+suffix, "tp_term_theirs_"+suffix
	for id, who := range map[string]string{mine: enroller, theirs: plain} {
		if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,owner_account,owner_team_id,name,launch_cwd) VALUES($1,$2,$3,$4,'work','/workspace')`, id, machine, who, team); err != nil {
			t.Fatal(err)
		}
	}
	if current, err = s.Get(ctx, owner, team); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GrantTerminalSession(ctx, enroller, team, TerminalSessionGrantRequest{OperationID: "share_terminal", ExpectedGeneration: current.Generation, TerminalSessionID: mine, Audience: "all_members", Role: "viewer", RuntimeGeneration: 1, Active: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.team_machine_requests(team_id,machine_id,requested_by,state) VALUES($1,$2,$3,'pending')`, team, machine, enroller); err != nil {
		t.Fatal(err)
	}

	// A plain member cannot return it.
	if err = act(plain, machine, "transfer_to_personal", "plain"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("plain member: %v", err)
	}
	// The exact machine id must be confirmed.
	if _, err = s.Machine(ctx, owner, team, MachineRequest{OperationID: "bad_confirm", ExpectedGeneration: current.Generation, MachineID: machine, Action: "transfer_to_personal", Confirmation: "other"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong confirmation: %v", err)
	}
	// The enrolling account must still be in the team.
	mutate(owner, MutationRequest{OperationID: "remove_enroller_of_orphan", Action: "remove", AccountID: gone})
	if err = act(owner, orphan, "transfer_to_personal", "orphan"); !errors.Is(err, ErrMachineOwnerLeft) {
		t.Fatalf("enroller left the team: %v", err)
	}
	// Team-owned previews or tunnels on the machine block it.
	tunnel := "tp_tunnel_" + suffix
	if _, err = store.SQL().ExecContext(ctx, `WITH endpoint AS (SELECT gen_random_uuid()::text AS id) INSERT INTO paperboat.tunnels(id,account_id,owner_team_id,name,desired_state,access_mode,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) SELECT $1,$2,$4,'team-tunnel','paused','team','endpoint_'||id,'https://'||id||'.example.test',$3,$2 FROM endpoint`, tunnel, enroller, machine, team); err != nil {
		t.Fatal(err)
	}
	if err = act(owner, machine, "transfer_to_personal", "served"); !errors.Is(err, ErrMachineTeamResources) {
		t.Fatalf("team tunnel on the machine: %v", err)
	}
	if _, err = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.tunnels WHERE id=$1`, tunnel); err != nil {
		t.Fatal(err)
	}
	preview := "tp_preview_" + suffix
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.preview_leases(owner_session_kind,id,endpoint,account_id,owner_team_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,access_mode,lease_deadline) VALUES('foreground',$1,'https://'||$1||'.example.test',$2,$3,$2,$4,'tp-owner-session','http','127.0.0.1:3000','team',now()+interval '1 hour')`, preview, enroller, team, machine); err != nil {
		t.Fatal(err)
	}
	if err = act(owner, machine, "transfer_to_personal", "previewed"); !errors.Is(err, ErrMachineTeamResources) {
		t.Fatalf("team preview on the machine: %v", err)
	}
	if _, err = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.preview_leases WHERE id=$1`, preview); err != nil {
		t.Fatal(err)
	}
	// A personal machine of the same account already named like it blocks it, as in the other direction.
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'laptop','linux','amd64','/workspace','offline','released')`, clash, enroller, "env_"+clash); err != nil {
		t.Fatal(err)
	}
	if err = act(owner, machine, "transfer_to_personal", "clash"); !errors.Is(err, ErrMachineAliasConflict) {
		t.Fatalf("alias in Personal: %v", err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET alias='renamed' WHERE id=$1`, clash); err != nil {
		t.Fatal(err)
	}

	var topicBefore, topicPlainBefore int64
	topic := `SELECT coalesce((SELECT generation FROM paperboat.account_topic_generations WHERE account_id=$1 AND topic='inventory'),0)`
	if err = store.SQL().QueryRowContext(ctx, topic, enroller).Scan(&topicBefore); err != nil {
		t.Fatal(err)
	}
	if err = store.SQL().QueryRowContext(ctx, topic, plain).Scan(&topicPlainBefore); err != nil {
		t.Fatal(err)
	}
	// An admin returns it: same id, back to the account that enrolled it.
	if err = act(enroller, machine, "transfer_to_personal", "back"); err != nil {
		t.Fatal(err)
	}
	var user, ownerTeam string
	var generation int
	if err = store.SQL().QueryRowContext(ctx, `SELECT user_id,coalesce(owner_team_id,''),installation_generation FROM paperboat.user_machines WHERE id=$1`, machine).Scan(&user, &ownerTeam, &generation); err != nil || user != enroller || ownerTeam != "" || generation != 7 {
		t.Fatalf("machine after return: %s %q %d %v", user, ownerTeam, generation, err)
	}
	allowed(enroller, "", machine, true)
	allowed(plain, team, machine, false)
	allowed(owner, team, machine, false)
	var active int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.team_resource_bindings WHERE team_id=$1 AND resource_id=$2 AND active`, team, machine).Scan(&active); err != nil || active != 0 {
		t.Fatalf("team binding after return: %d %v", active, err)
	}
	var requests, mineTeam, theirsTeam, liveBindings, liveGrants int
	if err = store.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM paperboat.team_machine_requests WHERE machine_id=$1),
 (SELECT count(*) FROM paperboat.user_machine_terminal_sessions WHERE id=$2 AND owner_team_id IS NULL),
 (SELECT count(*) FROM paperboat.user_machine_terminal_sessions WHERE id=$3 AND owner_team_id=$4),
 (SELECT count(*) FROM paperboat.team_resource_bindings WHERE team_id=$4 AND resource_kind='terminal_session' AND resource_id=$2 AND active),
 (SELECT count(*) FROM paperboat.team_terminal_session_grants WHERE team_id=$4 AND terminal_session_id=$2 AND active)`, machine, mine, theirs, team).Scan(&requests, &mineTeam, &theirsTeam, &liveBindings, &liveGrants); err != nil {
		t.Fatal(err)
	}
	if requests != 0 || mineTeam != 1 || theirsTeam != 1 || liveBindings != 0 || liveGrants != 0 {
		t.Fatalf("after return: requests=%d ownerSessionInPersonal=%d teammateSessionUntouched=%d liveBindings=%d liveGrants=%d", requests, mineTeam, theirsTeam, liveBindings, liveGrants)
	}
	// Peers: the account's other machines now see it as a personal peer; the team's members no longer see it.
	var audience []string
	if err = store.SQL().QueryRowContext(ctx, `SELECT paperboat.machine_audience_accounts($1)`, machine).Scan(&audience); err != nil || len(audience) != 1 || audience[0] != enroller {
		t.Fatalf("audience after return: %v %v", audience, err)
	}
	var topicAfter, topicPlainAfter int64
	if err = store.SQL().QueryRowContext(ctx, topic, enroller).Scan(&topicAfter); err != nil || topicAfter <= topicBefore {
		t.Fatalf("owner's inventory topic did not move: %d -> %d %v", topicBefore, topicAfter, err)
	}
	if err = store.SQL().QueryRowContext(ctx, topic, plain).Scan(&topicPlainAfter); err != nil || topicPlainAfter <= topicPlainBefore {
		t.Fatalf("team member's inventory topic did not move: %d -> %d %v", topicPlainBefore, topicPlainAfter, err)
	}
	// Returned once; the machine is no longer the team's.
	if err = act(owner, machine, "transfer_to_personal", "twice"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("second return: %v", err)
	}
}
