package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/billing"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
	"github.com/pinksaucepasta/paperboat-server/internal/testauth"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

// Exercise production session/cliAuth authentication and HTTP authorization. Only
// the external identity provider is substituted, as in the existing auth suite.
func TestTeamMachineAuthenticatedHTTPLifecycle(t *testing.T) {
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
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	writer := audit.NewWriter(store)
	authService := auth.NewService(store, writer, testauth.FakeWorkOSVerifier{}, []string{"test-session-key"}, true, "https://login.example.test")
	cliAuth := auth.NewDeviceService(store, writer, cfg.CLIAuth, []string{"test-cliAuth-hash-key"})
	cliAuth.ConfigureAuthorizationEncryptionKey("test-device-recovery-key")
	teamService := teams.NewService(store)
	router := authTestOriginHandler{next: NewRouter(Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService, DeviceAuth: cliAuth, Teams: teamService, WorkspaceBilling: billing.NewWorkspaceService(store, nil, ""), Machines: usermachines.New(store, writer, usermachines.Policy{}, nil)})}
	suffix := fmt.Sprint(time.Now().UnixNano())
	ownerEmail, memberEmail := "team-owner-"+suffix+"@example.test", "team-member-"+suffix+"@example.test"
	ownerCookies := loginCookies(t, router, "workos_owner_"+suffix+":"+ownerEmail+":Owner")
	memberCookies := loginCookies(t, router, "workos_member_"+suffix+":"+memberEmail+":Member")
	owner, member := userIDByEmail(t, store, ownerEmail), userIDByEmail(t, store, memberEmail)
	// The identity-provider fixture has no verified-email claim by default.
	// Email invitations require a verified recipient identity in production.
	if _, err := store.SQL().ExecContext(context.Background(), `UPDATE paperboat.users SET email_verified=true WHERE id=$1`, member); err != nil {
		t.Fatal(err)
	}
	ownerToken, memberToken := authorizeCLI(t, router, ownerCookies).AccessToken, authorizeCLI(t, router, memberCookies).AccessToken
	machine, teamID := "http_machine_"+suffix, "http-team-"+suffix
	if _, err := store.SQL().ExecContext(context.Background(), `INSERT INTO paperboat.user_machines(id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities) VALUES($1,$2,$3,'HTTP host','linux','amd64','/workspace','online','occupied',true,ARRAY['terminal_host'],ARRAY['terminal_host'])`, machine, owner, "env_"+suffix); err != nil {
		t.Fatal(err)
	}
	call := func(token, method, path string, body any, status int, out any) {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		// Single-machine reads are the dashboard's session-authenticated route;
		// team mutations above use the independently issued CLI bearer tokens.
		if method == http.MethodGet {
			req.Header.Del("Authorization")
			if token == ownerToken {
				addCookies(req, ownerCookies)
			} else if token == memberToken {
				addCookies(req, memberCookies)
			}
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("%s %s: status %d want %d", method, path, rec.Code, status)
		}
		if out != nil {
			var envelope struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(envelope.Data, out); err != nil {
				t.Fatal(err)
			}
		}
	}
	var team teams.Team
	call("", http.MethodPost, "/v1/teams", teams.CreateRequest{OperationID: "create", TeamID: teamID}, 401, nil)
	// Paid creation is covered by the billing owner; this fixture starts at
	// materialized Team state to exercise exact machine/grant HTTP authority.
	team, err = teamService.Create(context.Background(), owner, teams.CreateRequest{OperationID: "create", TeamID: teamID})
	if err != nil {
		t.Fatal(err)
	}
	// Shared machine grants require an active paid team, independently of membership.
	if _, err := store.SQL().ExecContext(context.Background(), `INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',2,now()-interval '1 hour',now()+interval '1 hour')`, "team-http-billing-"+suffix, teamID); err != nil {
		t.Fatal(err)
	}
	// Entitlement changes fence team generation; use the current HTTP projection.
	call(ownerToken, http.MethodGet, "/v1/teams/"+teamID, nil, 200, &team)
	// GET identity fields are visible to current members, with timestamps on the wire.
	var identity map[string]any
	call(ownerToken, http.MethodGet, "/v1/teams/"+teamID, nil, 200, &identity)
	members := identity["members"].([]any)
	first := members[0].(map[string]any)
	if first["email"] != ownerEmail || first["display_name"] != "Owner" || identity["display_name"] != teamID {
		t.Fatal("team identity contract missing")
	}
	if _, err := time.Parse(time.RFC3339Nano, first["joined_at"].(string)); err != nil {
		t.Fatal(err)
	}
	call(memberToken, http.MethodGet, "/v1/teams/"+teamID, nil, 403, nil)
	call(ownerToken, http.MethodPost, "/v1/teams/"+teamID+"/actions", map[string]any{"operation_id": "rename", "expected_generation": team.Generation, "action": "rename", "name": "Renamed Team"}, 200, &team)
	if team.Name != "Renamed Team" {
		t.Fatal("rename not applied")
	}
	var invite teams.Invitation
	call(ownerToken, http.MethodPost, "/v1/teams/"+teamID+"/invitations", teams.InviteRequest{OperationID: "invite", ExpectedGeneration: team.Generation, Email: memberEmail, Role: "admin"}, 200, &invite)
	var invitation map[string]any
	call(ownerToken, http.MethodGet, "/v1/team-invitations/"+invite.InvitationID, nil, 200, &invitation)
	if invitation["email"] != memberEmail || invitation["display_name"] != "Member" || invitation["created_by_email"] != ownerEmail || invitation["role"] != "admin" {
		t.Fatal("invitation identity contract missing")
	}
	if _, err := time.Parse(time.RFC3339Nano, invitation["created_at"].(string)); err != nil {
		t.Fatal(err)
	}
	call(memberToken, http.MethodPost, "/v1/team-invitations/"+invite.InvitationID+"/accept", teams.AcceptRequest{OperationID: "accept"}, 200, &team)
	for _, m := range team.Members {
		if m.AccountID == member && m.Role != "admin" {
			t.Fatal("invited role lost on acceptance")
		}
	}
	call(memberToken, http.MethodPost, "/v1/teams/"+teamID+"/invitations", map[string]any{"operation_id": "escalating_invite", "expected_generation": team.Generation, "email": "another@example.test", "role": "admin"}, 403, nil)
	call(ownerToken, http.MethodPost, "/v1/teams/"+teamID+"/invitations", map[string]any{"operation_id": "invalid_role", "expected_generation": team.Generation, "email": "another@example.test", "role": "owner"}, 400, nil)
	call(ownerToken, http.MethodPost, "/v1/teams/"+teamID+"/actions", teams.MutationRequest{OperationID: "initial_demote", ExpectedGeneration: team.Generation, Action: "role", AccountID: member, Role: "member"}, 200, &team)
	call(memberToken, http.MethodGet, "/v1/teams/"+teamID, nil, 200, &identity)
	call(ownerToken, http.MethodPost, "/v1/teams/"+teamID+"/actions", teams.MutationRequest{OperationID: "identity_remove", ExpectedGeneration: team.Generation, Action: "remove", AccountID: member}, 200, &team)
	call(memberToken, http.MethodGet, "/v1/teams/"+teamID, nil, 403, nil)
	call(ownerToken, http.MethodPost, "/v1/teams/"+teamID+"/invitations", teams.InviteRequest{OperationID: "identity_reinvite", ExpectedGeneration: team.Generation, Email: memberEmail}, 200, &invite)
	call(memberToken, http.MethodPost, "/v1/team-invitations/"+invite.InvitationID+"/accept", teams.AcceptRequest{OperationID: "identity_reaccept"}, 200, &team)
	for _, m := range team.Members {
		if m.AccountID == member && m.Role != "member" {
			t.Fatal("default invitation role is not member")
		}
	}

	call(ownerToken, http.MethodPost, "/v1/teams/"+teamID+"/machines", teams.MachineRequest{OperationID: "share", ExpectedGeneration: team.Generation, MachineID: machine, Action: "share"}, 200, &team)
	call(memberToken, http.MethodGet, "/v1/machines/"+machine, nil, 404, nil)
	call(ownerToken, http.MethodPost, "/v1/teams/"+teamID+"/machine-grants", teams.MachineGrantRequest{OperationID: "grant", ExpectedGeneration: team.Generation, MachineID: machine, Audience: "selected_member", AccountID: member, Capabilities: []string{"terminal"}, Active: true}, 200, &team)
	var visible usermachines.UserMachine
	call(memberToken, http.MethodGet, "/v1/machines/"+machine, nil, 200, &visible)
	if visible.CanManage || len(visible.Permissions) != 1 || visible.Permissions[0] != "terminal" {
		t.Fatal("HTTP permissions do not match exact grant")
	}
	call(memberToken, http.MethodPost, "/v1/teams/"+teamID+"/machine-grants", teams.MachineGrantRequest{OperationID: "escalate", ExpectedGeneration: team.Generation, MachineID: machine, Audience: "all_members", Capabilities: []string{"exec"}, Active: true}, 403, nil)
	call(ownerToken, http.MethodPost, "/v1/teams/"+teamID+"/machines", teams.MachineRequest{OperationID: "unshare", ExpectedGeneration: team.Generation, MachineID: machine, Action: "unshare"}, 200, &team)
	call(memberToken, http.MethodGet, "/v1/machines/"+machine, nil, 404, nil)
	call(ownerToken, http.MethodGet, "/v1/machines/"+machine, nil, 200, &visible)
	if !visible.CanManage {
		t.Fatal("withdrawal removed owner control")
	}

	var activity teams.ActivityPage
	activityPath := "/v1/teams/" + teamID + "/activity?limit=2"
	call("", http.MethodGet, activityPath, nil, 401, nil)
	call(memberToken, http.MethodGet, activityPath, nil, 403, nil)
	call(ownerToken, http.MethodGet, activityPath, nil, 200, &activity)
	if len(activity.Items) != 2 || activity.NextCursor == "" {
		t.Fatal("activity pagination missing")
	}
	call(ownerToken, http.MethodPost, "/v1/teams/"+teamID+"/actions", teams.MutationRequest{OperationID: "appoint", ExpectedGeneration: team.Generation, Action: "role", AccountID: member, Role: "admin"}, 200, &team)
	call(memberToken, http.MethodGet, activityPath, nil, 200, &activity)
	call(memberToken, http.MethodPost, "/v1/teams/"+teamID+"/actions", teams.MutationRequest{OperationID: "admin_delete", ExpectedGeneration: team.Generation, Action: "delete", Confirmation: teamID}, 403, nil)
	call(ownerToken, http.MethodPost, "/v1/teams/"+teamID+"/actions", teams.MutationRequest{OperationID: "demote", ExpectedGeneration: team.Generation, Action: "role", AccountID: member, Role: "member"}, 200, &team)
	call(memberToken, http.MethodGet, activityPath+"&cursor="+activity.NextCursor, nil, 403, nil)
	call(ownerToken, http.MethodPost, "/v1/teams/"+teamID+"/actions", teams.MutationRequest{OperationID: "owner_leave", ExpectedGeneration: team.Generation, Action: "leave"}, 403, nil)
	call(ownerToken, http.MethodPost, "/v1/teams/"+teamID+"/actions", teams.MutationRequest{OperationID: "transfer", ExpectedGeneration: team.Generation, Action: "transfer", AccountID: member}, 200, &team)
	call(ownerToken, http.MethodGet, activityPath, nil, 403, nil)
	call(memberToken, http.MethodGet, activityPath, nil, 200, &activity)
	call(ownerToken, http.MethodPost, "/v1/teams/"+teamID+"/actions", teams.MutationRequest{OperationID: "leave", ExpectedGeneration: team.Generation, Action: "leave"}, 200, &team)
	call(ownerToken, http.MethodGet, "/v1/teams/"+teamID, nil, 403, nil)
	call(memberToken, http.MethodPost, "/v1/teams/"+teamID+"/actions", teams.MutationRequest{OperationID: "delete", ExpectedGeneration: team.Generation, Action: "delete", Confirmation: teamID}, 200, &team)
	call(memberToken, http.MethodGet, activityPath, nil, 404, nil)
}
