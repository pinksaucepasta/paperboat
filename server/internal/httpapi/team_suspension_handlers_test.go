package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

type suspendedTeamHTTPStub struct {
	teamAPI
	calls int
	err   error
}

func (s *suspendedTeamHTTPStub) Mutate(context.Context, string, string, teams.MutationRequest) (teams.Team, error) {
	s.calls++
	return teams.Team{}, s.err
}
func TestTeamMutationSuspensionHTTP(t *testing.T) {
	for _, tc := range []struct {
		name          string
		authenticated bool
		err           error
		status        int
		code          string
	}{
		{"anonymous", false, teams.ErrSuspended, 401, "unauthenticated"},
		{"outsider", true, teams.ErrForbidden, 403, "forbidden"},
		{"current member", true, teams.ErrSuspended, 409, "team_suspended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &suspendedTeamHTTPStub{err: tc.err}
			mux := http.NewServeMux()
			pass := func(h http.Handler) http.Handler { return h }
			registerTeamRoutes(mux, s, pass, pass)
			r := httptest.NewRequest(http.MethodPost, "/v1/teams/team-a/actions", strings.NewReader(`{"operation_id":"rename","expected_generation":2,"action":"rename","name":"Team"}`))
			if tc.authenticated {
				r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "actor"}}))
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			var body ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status || body.Error.Code != tc.code {
				t.Fatalf("status=%d code=%s want %d/%s", w.Code, body.Error.Code, tc.status, tc.code)
			}
			if !tc.authenticated && s.calls != 0 {
				t.Fatal("anonymous request reached team service")
			}
			if tc.code == "team_suspended" && !strings.Contains(body.Error.Message, "team owner") {
				t.Fatal("suspension error has no recovery")
			}
		})
	}
}
