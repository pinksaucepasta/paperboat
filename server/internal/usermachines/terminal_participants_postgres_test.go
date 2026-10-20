package usermachines

import (
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/helperruntime"
)

func TestTerminalParticipantsAreNamedOnlyForTheOwnerAndThemselvesOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	prefix := "participants_" + fmt.Sprint(time.Now().UnixNano()) + "_"
	owner, mate, outsider, team := prefix+"owner", prefix+"mate", prefix+"outsider", prefix+"team"
	machine, session, guest := prefix+"machine", prefix+"session", prefix+"guest"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, command := range []struct{ query, arg string }{
			{`DELETE FROM paperboat.user_machine_terminal_sessions WHERE id=$1`, session},
			{`DELETE FROM paperboat.user_machines WHERE id=$1`, machine},
			{`DELETE FROM paperboat.team_members WHERE team_id=$1`, team},
			{`DELETE FROM paperboat.teams WHERE team_id=$1`, team},
		} {
			_, _ = store.SQL().ExecContext(t.Context(), command.query, command.arg)
		}
		_, _ = store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.users WHERE id IN ($1,$2,$3)`, owner, mate, outsider)
	})
	for id, name := range map[string]string{owner: "Ada Owner", mate: "Bo Mate", outsider: "Cy Outsider"} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,email_verified,display_name,status) VALUES($1,$1,$2,true,$3,'active')`, id, id+"@example.test", name)
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)`, team, owner, mate)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$1,'participants','linux','amd64','/workspace','online','occupied')`, machine, owner)
	exec(`INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,name,launch_cwd,owner_account) VALUES($1,$2,'shared','/workspace',$3)`, session, machine, owner)
	exec(`INSERT INTO paperboat.terminal_code_access(terminal_session_id,generation,enabled,code_hash,role,expires_at) VALUES($1,1,true,decode(repeat('ab',32),'hex'),'viewer',now()+interval '1 hour')`, session)
	exec(`INSERT INTO paperboat.terminal_guest_sessions(id,token_hash,terminal_session_id,code_generation,display_name,expires_at) VALUES($1,decode(repeat('cd',32),'hex'),$2,1,'Guesty',now()+interval '1 hour')`, guest, session)
	service := New(store, nil, Policy{}, nil)
	live := []helperruntime.Participant{
		{AttachmentID: "a1", AccountID: owner, ClientID: "c1", Role: "owner"},
		{AttachmentID: "a2", AccountID: mate, ClientID: "c2", Role: "interactive"},
		{AttachmentID: "a3", AccountID: outsider, ClientID: "c3", Role: "viewer"},
		{AttachmentID: "a4", AccountID: guest, ClientID: "c4", Role: "viewer"},
	}
	type shown struct{ kind, name, email string }
	describe := func(viewer string) map[string]shown {
		t.Helper()
		got, err := service.describeParticipants(ctx, viewer, owner, session, live)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]shown{}
		for _, participant := range got {
			out[participant.AccountID] = shown{participant.Kind, participant.DisplayName, participant.Email}
		}
		return out
	}
	// The owner sees everyone's name; emails only where the owner may already know them: their own and a
	// team mate's, not an unrelated account's, and a guest has none.
	want := map[string]shown{
		owner: {"member", "Ada Owner", owner + "@example.test"}, mate: {"member", "Bo Mate", mate + "@example.test"},
		outsider: {"member", "Cy Outsider", ""}, guest: {"guest", "Guesty", ""},
	}
	for id, w := range want {
		if got := describe(owner)[id]; got != w {
			t.Errorf("owner sees %s as %+v, want %+v", id, got, w)
		}
	}
	// An address the owner allow-listed for this terminal is known to the owner.
	exec(`UPDATE paperboat.terminal_browser_access_policies SET audience='specific_emails',team_id=NULL,allowed_emails=ARRAY[$2]::text[],interactive_emails=ARRAY[]::text[] WHERE terminal_session_id=$1`, session, outsider+"@example.test")
	if got := describe(owner)[outsider]; got.email != outsider+"@example.test" {
		t.Errorf("an allow-listed participant's email was hidden from the owner: %+v", got)
	}
	// Anyone else sees kinds, never names: only their own entry is described.
	for _, viewer := range []string{mate, outsider, guest} {
		seen := describe(viewer)
		for id, entry := range seen {
			if id == viewer {
				if entry.name == "" {
					t.Errorf("%s does not see their own name", viewer)
				}
				continue
			}
			if entry.name != "" || entry.email != "" {
				t.Errorf("%s was shown %s as %+v", viewer, id, entry)
			}
			if entry.kind == "" {
				t.Errorf("kind missing for %s", id)
			}
		}
	}
	if seen := describe(mate)[guest]; seen.kind != "guest" {
		t.Errorf("a guest looked like %q to a member", seen.kind)
	}
}

func TestOwnerRemovesAGuestWhoJoinedWithTheCodeOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	prefix := "guestrm_" + fmt.Sprint(time.Now().UnixNano()) + "_"
	owner, other := prefix+"owner", prefix+"other"
	machine, session, guest, kept := prefix+"machine", prefix+"session", prefix+"guest", prefix+"kept"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.user_machine_terminal_sessions WHERE id=$1`, session)
		_, _ = store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		_, _ = store.SQL().ExecContext(t.Context(), `DELETE FROM paperboat.users WHERE id IN ($1,$2)`, owner, other)
	})
	for _, id := range []string{owner, other} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,email_verified,status) VALUES($1,$1,$2,true,'active')`, id, id+"@example.test")
	}
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$1,'guestrm','linux','amd64','/workspace','online','occupied')`, machine, owner)
	exec(`INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,name,launch_cwd,owner_account) VALUES($1,$2,'shared','/workspace',$3)`, session, machine, owner)
	exec(`INSERT INTO paperboat.terminal_code_access(terminal_session_id,generation,enabled,code_hash,role,expires_at) VALUES($1,1,true,decode(repeat('ab',32),'hex'),'viewer',now()+interval '1 hour')`, session)
	for _, id := range []string{guest, kept} {
		exec(`INSERT INTO paperboat.terminal_guest_sessions(id,token_hash,terminal_session_id,code_generation,display_name,expires_at) VALUES($1,decode(md5($1)||md5($1),'hex'),$2,1,'Guesty',now()+interval '1 hour')`, id, session)
	}
	service := New(store, nil, Policy{}, nil)
	count := func() (n int) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.terminal_guest_sessions WHERE terminal_session_id=$1`, session).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return
	}
	// Only the terminal's owner may remove anyone; the guest stays.
	if _, err := service.RemoveTerminalParticipant(ctx, other, session, guest, "op", 0); err == nil || count() != 2 {
		t.Fatalf("a stranger removed a guest: err=%v guests=%d", err, count())
	}
	if _, err := service.RemoveTerminalParticipant(ctx, owner, session, guest, "op", 0); err != nil {
		t.Fatalf("owner removing a guest: %v", err)
	}
	if count() != 1 {
		t.Fatalf("guest sessions after removal = %d, want only the other guest left", count())
	}
	var left string
	if err := store.SQL().QueryRowContext(ctx, `SELECT id FROM paperboat.terminal_guest_sessions WHERE terminal_session_id=$1`, session).Scan(&left); err != nil || left != kept {
		t.Fatalf("remaining guest = %q %v, want %q", left, err, kept)
	}
}
