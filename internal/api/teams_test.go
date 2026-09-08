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
