package teaminbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

// Sends from a team-owned machine (Lane O, F12: `pb send` from laneo-pa to laneo-pb answered 403 after both
// moved to the team). The source follows the descriptor rule (own personal machine or a machine of a team the
// sender belongs to). Between machines the sender enrolled, no approval is needed; to a machine another member
// enrolled, that member approves (a departed enroller makes it forbidden, see the lifecycle test); a non-member
// cannot send from the team's machine.
func TestPostgresMemberSendsBetweenTeamOwnedMachines(t *testing.T) {
	store := inboxTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, member, stranger, team := "tt_owner_"+suffix, "tt_member_"+suffix, "tt_stranger_"+suffix, "tt-team-"+suffix
	source, destination := "tt_source_"+suffix, "tt_destination_"+suffix
	for _, account := range []string{owner, member, stranger} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, account); err != nil {
			t.Fatal(err)
		}
	}
	for _, machine := range []string{source, destination} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities) VALUES($1,$2,$3,$1,'linux','amd64','/workspace','online','occupied',true,ARRAY['file_receive'],ARRAY['file_receive'])`, machine, owner, "env_"+machine); err != nil {
			t.Fatal(err)
		}
	}
	authority := teams.NewService(store)
	current, err := authority.Create(ctx, owner, teams.CreateRequest{OperationID: "create_" + suffix, TeamID: team})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,state,quantity,period_start,period_end) VALUES($1,$2,'active',3,now()-interval '1 day',now()+interval '30 days') ON CONFLICT(id) DO UPDATE SET state='active',quantity=3,period_start=EXCLUDED.period_start,period_end=EXCLUDED.period_end`, "team:"+team, team); err != nil {
		t.Fatal(err)
	}
	if current, err = authority.Get(ctx, owner, team); err != nil {
		t.Fatal(err)
	}
	invite, err := authority.Invite(ctx, owner, team, teams.InviteRequest{OperationID: "invite_" + suffix, ExpectedGeneration: current.Generation, AccountID: member})
	if err != nil {
		t.Fatal(err)
	}
	if current, err = authority.Accept(ctx, member, invite.InvitationID, teams.AcceptRequest{OperationID: "accept_" + suffix}); err != nil {
		t.Fatal(err)
	}
	for _, machine := range []string{source, destination} {
		if current, err = authority.Machine(ctx, owner, team, teams.MachineRequest{OperationID: "transfer_" + machine, ExpectedGeneration: current.Generation, MachineID: machine, Action: "transfer_to_team", Confirmation: machine}); err != nil {
			t.Fatalf("transfer %s: %v", machine, err)
		}
	}
	if current, err = authority.GrantMachine(ctx, owner, team, teams.MachineGrantRequest{OperationID: "grant_" + suffix, ExpectedGeneration: current.Generation, MachineID: destination, Audience: "all_members", Capabilities: []string{"files"}, Active: true}); err != nil {
		t.Fatal(err)
	}
	service, _ := New(store, config.Default().Access.FileTransfer)
	files := []File{{Basename: "proof.txt", Size: 5, SHA256: strings.Repeat("b", 64)}}
	send := func(sender, id string) (Request, error) {
		return service.Create(ctx, RequestInput{RequestID: "tir_" + id + "_" + suffix, OperationID: "op_" + id + "_" + suffix, SenderAccount: sender, SourceMachineID: source, DestinationMachineID: destination, BatchID: "fb_" + id + "_" + suffix, Files: files, ExpiresAt: time.Now().UTC().Add(time.Hour)})
	}
	// The enroller of both team machines (Lane O's case) sends between them without an approval step.
	request, err := send(owner, "enroller")
	if err != nil {
		t.Fatalf("enroller send between their own team-owned machines: %v", err)
	}
	if request.Status != "not_required" {
		t.Fatalf("send between one's own team machines needs no approval, got %q", request.Status)
	}
	// Another member may send from the team machine too; it goes to the destination's enroller for approval.
	if request, err = send(member, "member"); err != nil || request.Status != "pending" || request.RecipientAccount != owner {
		t.Fatalf("member send to the enroller's team machine = %+v, %v; want pending for the enroller", request, err)
	}
	if _, err = send(stranger, "stranger"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-member send from the team's machine = %v, want forbidden", err)
	}
}
