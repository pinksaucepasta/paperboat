package httpapi

import (
	"context"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/environment"
	"net/http/httptest"
	"testing"
)

type vaultTeamReadStub struct {
	environmentVaultScopesAPI
	calls int
	err   error
}

func (s *vaultTeamReadStub) GetVaultTeam(context.Context, string, string) (environment.VaultTeamState, error) {
	s.calls++
	return environment.VaultTeamState{}, s.err
}
func TestVaultTeamMissingReturns404WithoutMaskingConflicts(t *testing.T) {
	for _, tc := range []struct {
		scope         string
		err           error
		status, calls int
	}{
		{"audit-team", environment.ErrNotFound, 404, 1},
		{"personal", environment.ErrNotFound, 409, 0},
		{"audit-team", environment.ErrVersionConflict, 409, 1},
		{"audit-team", environment.ErrKeyAuthorizationRequired, 403, 1},
	} {
		service := &vaultTeamReadStub{err: tc.err}
		r := httptest.NewRequest("GET", "/v1/environment/teams/audit-team?workspace="+tc.scope, nil)
		r.SetPathValue("team_id", "audit-team")
		r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "owner"}}))
		w := httptest.NewRecorder()
		environmentVaultTeamHandler(service, "get")(w, r)
		if w.Code != tc.status || service.calls != tc.calls {
			t.Fatalf("scope%s err%v status%d calls%d", tc.scope, tc.err, w.Code, service.calls)
		}
	}
}
