package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
)

func TestConfigCompareConnectionRejectsBrowserPrincipal(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/config-sync/environments/env_1/conflicts/revision/connection", strings.NewReader(`{"assignment_version":1,"path":"settings/editor","expected_remote_revision":"remote"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "account_1"}}))
	response := httptest.NewRecorder()

	configCompareConnection(nil, nil).ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("browser principal status = %d, want %d", response.Code, http.StatusForbidden)
	}
}
