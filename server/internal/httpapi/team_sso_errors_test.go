package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// PLAN F19-04: a missing SSO sign-in is refused as team_sso_required on every workspace error path
// and still counts as forbidden for callers that only know ErrForbidden.
func TestTeamSSORequiredMapsEverywhere(t *testing.T) {
	if !errors.Is(workspace.ErrSSORequired, workspace.ErrForbidden) || !errors.Is(workspace.ErrSSORequired, teams.ErrSSORequired) {
		t.Fatal("SSO error must also be forbidden and teams.ErrSSORequired")
	}
	for name, write := range map[string]func(http.ResponseWriter, *http.Request, error){
		"workspace": writeWorkspaceError,
		"preview":   writePreviewLeaseError,
		"tunnel":    writeTunnelError,
		"favorite":  func(w http.ResponseWriter, r *http.Request, err error) { writeFavoriteWorkspaceError(w, r, err) },
	} {
		w := httptest.NewRecorder()
		write(w, httptest.NewRequest("GET", "/v1/machines", nil), workspace.ErrSSORequired)
		if w.Code != 403 || !strings.Contains(w.Body.String(), `"team_sso_required"`) {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
}
