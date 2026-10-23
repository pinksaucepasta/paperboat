package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

func TestTeamHTTPPrincipalAndStrictBody(t *testing.T) {
	service := billing.NewWorkspaceService(nil, nil, "")
	handler := paidTeamCreation(service, []string{"https://console.example.test"})
	valid := `{"operation_id":"op_a","team_id":"team-a","seats":1,"interval":"month","success_url":"https://console.example.test/success","cancel_url":"https://console.example.test/cancel"}`
	for _, tc := range []struct {
		name, body, key string
		authenticated   bool
		status          int
	}{
		{"anonymous", valid, "op_a", false, 401},
		{"actor substitution", strings.TrimSuffix(valid, "}") + `,"account_id":"victim"}`, "op_a", true, 400},
		{"bounded body", `{"operation_id":"op_a","team_id":"` + strings.Repeat("a", 17<<10) + `"}`, "op_a", true, 400},
		{"free creation rejected", `{"operation_id":"op_a","team_id":"team-a"}`, "op_a", true, 400},
		{"missing idempotency header", valid, "", true, 400},
		{"different idempotency header", valid, "op_b", true, 400},
		{"external return origin", strings.ReplaceAll(valid, "console.example.test", "attacker.example.test"), "op_a", true, 400},
		{"valid purchase reaches billing provider guard", valid, "op_a", true, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/teams", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", tc.key)
			if tc.authenticated {
				r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "current_actor"}}))
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d", w.Code, tc.status)
			}
		})
	}
}

type machineTeamHTTPStub struct {
	teamAPI
	calls       int
	actor, team string
	grant       teams.MachineGrantRequest
	machine     teams.MachineRequest
}

func (s *machineTeamHTTPStub) GrantMachine(_ context.Context, actor, team string, in teams.MachineGrantRequest) (teams.Team, error) {
	s.calls++
	s.actor = actor
	s.team = team
	s.grant = in
	return teams.Team{TeamID: team}, nil
}
func (s *machineTeamHTTPStub) Machine(_ context.Context, actor, team string, in teams.MachineRequest) (teams.Team, error) {
	s.calls++
	s.actor = actor
	s.team = team
	s.machine = in
	return teams.Team{TeamID: team}, nil
}
func TestTeamMachineHTTPExactResourceAndActor(t *testing.T) {
	s := &machineTeamHTTPStub{}
	mux := http.NewServeMux()
	pass := func(h http.Handler) http.Handler { return h }
	registerTeamRoutes(mux, s, pass, pass)
	for _, tc := range []struct {
		path, body    string
		authenticated bool
		status        int
	}{
		{"/machine-grants", `{"operation_id":"grant","expected_generation":3,"machine_id":"machine_a","audience":"selected_member","account_id":"recipient","capabilities":["exec"],"active":true}`, true, 200},
		{"/machines", `{"operation_id":"share","expected_generation":4,"machine_id":"machine_a","action":"share"}`, true, 200},
		{"/machines", `{"operation_id":"share","expected_generation":4,"machine_id":"machine_a","action":"share","account_id":"victim"}`, true, 400},
		{"/machine-grants", `{"operation_id":"grant"}`, false, 401},
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/teams/team_a"+tc.path, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		if tc.authenticated {
			r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "actor"}}))
		}
		w := httptest.NewRecorder()
		before := s.calls
		mux.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s: status %d want %d", tc.path, w.Code, tc.status)
		}
		if tc.status != 200 && s.calls != before {
			t.Fatal("invalid request reached machine service")
		}
	}
	if s.calls != 2 || s.actor != "actor" || s.team != "team_a" || s.grant.AccountID != "recipient" || len(s.grant.Capabilities) != 1 || s.grant.Capabilities[0] != "exec" || s.machine.MachineID != "machine_a" {
		t.Fatal("machine request authority or exact capability lost")
	}
}

type activityTeamHTTPStub struct {
	teamAPI
	calls               int
	actor, team, cursor string
	limit               int
	err                 error
}

func (s *activityTeamHTTPStub) Activity(_ context.Context, actor, team, cursor string, limit int) (teams.ActivityPage, error) {
	s.calls++
	s.actor = actor
	s.team = team
	s.cursor = cursor
	s.limit = limit
	return teams.ActivityPage{Items: []teams.ActivityItem{}}, s.err
}
func TestTeamActivityHTTP(t *testing.T) {
	s := &activityTeamHTTPStub{}
	mux := http.NewServeMux()
	pass := func(h http.Handler) http.Handler { return h }
	registerTeamRoutes(mux, s, pass, pass)
	for _, tc := range []struct {
		query         string
		authenticated bool
		err           error
		status        int
	}{
		{"", false, nil, 401}, {"?limit=201", true, nil, 400}, {"?limit=-1", true, nil, 400}, {"?limit=no", true, nil, 400},
		{"?limit=0", true, nil, 400}, {"?limit=2&cursor=bound", true, nil, 200}, {"", true, teams.ErrInvalid, 400},
		{"", true, teams.ErrForbidden, 403}, {"", true, teams.ErrNotFound, 404}, {"", true, teams.ErrConflict, 409},
	} {
		r := httptest.NewRequest(http.MethodGet, "/v1/teams/team_a/activity"+tc.query, nil)
		if tc.authenticated {
			r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "current_actor"}}))
		}
		s.err = tc.err
		before := s.calls
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("query=%s status=%d want=%d", tc.query, w.Code, tc.status)
		}
		if tc.status == 401 || tc.status == 400 && tc.err == nil {
			if before != s.calls {
				t.Fatal("invalid request reached service")
			}
		}
		if tc.status == 200 && (s.actor != "current_actor" || s.team != "team_a" || s.limit != 2 || s.cursor != "bound") {
			t.Fatal("lost principal or pagination")
		}
	}
}

func TestTeamActivityHTTPDoesNotMapJoinedOperationalFailureToNotFound(t *testing.T) {
	s := &activityTeamHTTPStub{err: errors.Join(teams.ErrNotFound, syscall.EIO, errors.New("TEAM_DATABASE_SECRET"))}
	mux := http.NewServeMux()
	pass := func(h http.Handler) http.Handler { return h }
	registerTeamRoutes(mux, s, pass, pass)
	request, logs, reference := diagnosticRequest("/v1/teams/team_a/activity")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "current_actor"}}))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want operational failure", response.Code, response.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "internal_error" || body.Error.SupportReference != reference || body.Error.RequestID != "request_fault_01" || body.Error.CorrelationID != "correlation_fault_01" {
		t.Fatalf("response lost safe error identity: %+v", body.Error)
	}
	if strings.Contains(response.Body.String(), "TEAM_DATABASE_SECRET") || strings.Contains(logs.String(), "TEAM_DATABASE_SECRET") {
		t.Fatal("team operation failure exposed private error text")
	}
	decoder := json.NewDecoder(bytes.NewReader(logs.Bytes()))
	var event map[string]any
	if err := decoder.Decode(&event); err != nil {
		t.Fatalf("decode failure log: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("expected one correlated failure log, extra=%v err=%v", extra, err)
	}
	if event["code"] != "internal_error" || event["cause"] != "system_error" || event["errno"] != float64(syscall.EIO) || event["support_reference"] != reference || event["correlation_id"] != "correlation_fault_01" || event["http_status"] != float64(http.StatusInternalServerError) {
		t.Fatalf("failure log lost original cause or correlation: %+v", event)
	}
}
