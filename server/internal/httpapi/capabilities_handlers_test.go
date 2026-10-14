package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
)

func TestMachineCapabilitiesRejectsRetiredPeerRelayField(t *testing.T) {
	ctx := context.WithValue(context.Background(), authContextKey{}, principal{User: auth.User{ID: "account_test"}})
	request := httptest.NewRequest(http.MethodPut, "/v1/machines/machine_test/capabilities", strings.NewReader(`{"expected_version":1,"desired":{"terminal":true,"peer_relay":true}}`)).WithContext(ctx)
	response := httptest.NewRecorder()
	userMachineCapabilities(nil).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("retired peer_relay field status=%d body=%s", response.Code, response.Body.String())
	}
}
