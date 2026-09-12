package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestTeamClientRoutesAndIdempotency(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		if calls == 1 {
			if r.Method != http.MethodPost || r.URL.Path != "/v1/teams/team_1/invitations" || r.Header.Get("Idempotency-Key") != "op_1" {
				t.Errorf("request=%s %s idempotency=%q", r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"))
			}
			var in TeamInviteRequest
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.ExpectedGeneration != 7 || in.AccountID != "acct_2" {
				t.Errorf("input=%+v", in)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": TeamInvitation{InvitationID: "inv_1", TeamID: "team_1", AccountID: "acct_2", Role: "member"}})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/teams/team_1/resources" || r.Header.Get("Idempotency-Key") != "op_2" {
			t.Errorf("request=%s %s idempotency=%q", r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": Team{TeamID: "team_1", Generation: 8}})
	}))
	defer server.Close()
	c := New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	invite, err := c.InviteTeamMember(context.Background(), "team_1", TeamInviteRequest{OperationID: "op_1", ExpectedGeneration: 7, AccountID: "acct_2"})
	if err != nil || invite.Role != "member" {
		t.Fatalf("invite=%+v err=%v", invite, err)
	}
	team, err := c.AttachTeamResource(context.Background(), "team_1", TeamAttachRequest{OperationID: "op_2", ExpectedGeneration: 7, ResourceKind: "preview", ResourceID: "prv_1", Active: true})
	if err != nil || team.Generation != 8 {
		t.Fatalf("team=%+v err=%v", team, err)
	}
}

func TestTeamMachineClientExactCapabilityAndOwnershipRoutes(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer token" || r.Method != http.MethodPost {
			t.Error("authenticated mutation lost")
		}
		switch calls {
		case 1:
			if r.URL.Path != "/v1/teams/team_1/machine-grants" || r.Header.Get("Idempotency-Key") != "grant_1" {
				t.Errorf("grant route: %s", r.URL.Path)
			}
			var in TeamMachineGrantRequest
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Fatal(err)
			}
			if in.MachineID != "machine_1" || in.Audience != "selected_member" || in.AccountID != "member_1" || len(in.Capabilities) != 1 || in.Capabilities[0] != "exec" || in.ExpectedGeneration != 4 {
				t.Errorf("exact grant lost: %+v", in)
			}
		case 2:
			if r.URL.Path != "/v1/teams/team_1/machines" || r.Header.Get("Idempotency-Key") != "transfer_1" {
				t.Errorf("machine route: %s", r.URL.Path)
			}
			var in TeamMachineRequest
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Fatal(err)
			}
			if in.Action != "transfer_to_team" || in.Confirmation != "machine_1" || in.ExpectedGeneration != 5 {
				t.Errorf("ownership intent lost: %+v", in)
			}
		default:
			t.Error("unexpected request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": Team{TeamID: "team_1", Generation: 6, Machines: []TeamMachineBinding{{MachineID: "machine_1", OwnerTeamID: "team_1"}}}})
	}))
	defer server.Close()
	c := New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	if _, err := c.GrantTeamMachine(context.Background(), "team_1", TeamMachineGrantRequest{OperationID: "grant_1", ExpectedGeneration: 4, MachineID: "machine_1", Audience: "selected_member", AccountID: "member_1", Capabilities: []string{"exec"}, Active: true}); err != nil {
		t.Fatal(err)
	}
	out, err := c.MutateTeamMachine(context.Background(), "team_1", TeamMachineRequest{OperationID: "transfer_1", ExpectedGeneration: 5, MachineID: "machine_1", Action: "transfer_to_team", Confirmation: "machine_1"})
	if err != nil || len(out.Machines) != 1 || out.Machines[0].OwnerTeamID != "team_1" || out.Machines[0].OwnerAccount != "" {
		t.Fatalf("ownership response: %+v %v", out, err)
	}
}
