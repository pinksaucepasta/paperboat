package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/breakglass"
	"github.com/pinksaucepasta/paperboat/server/internal/operatorauth"
)

// Each refusal of a break-glass start has its own status and code, so the panel can say what to fix.
func TestBreakGlassErrorsMapToTheirStatus(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{breakglass.ErrPhrase, http.StatusBadRequest, "validation_failed"},
		{operatorauth.ErrMFAInvalid, http.StatusForbidden, "mfa_invalid"},
		{&breakglass.NotReadyError{Checks: []breakglass.Check{{Name: "mint signing keys"}}}, http.StatusConflict, "breakglass_not_ready"},
		{breakglass.ErrRunning, http.StatusConflict, "breakglass_conflict"},
		{breakglass.ErrNotFound, http.StatusNotFound, "not_found"},
		{errors.New("database down"), http.StatusServiceUnavailable, "breakglass_unavailable"},
	} {
		w := httptest.NewRecorder()
		if !breakGlassError(w, httptest.NewRequest(http.MethodPost, "/", nil), tc.err) || w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
			t.Errorf("%v: status %d body %s", tc.err, w.Code, w.Body.String())
		}
	}
	if breakGlassError(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), nil) {
		t.Error("no error must write nothing")
	}
}
