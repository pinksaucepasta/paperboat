package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/environment"
)

type vaultScopesHTTPStub struct {
	*environment.Service
	account, kind, owner, machine string
	calls                         int
	err                           error
}

func (s *vaultScopesHTTPStub) PutVaultScope(_ context.Context, account, workspaceID, kind, owner, machine string, _ environment.VaultScopePut) (environment.VaultScopeState, error) {
	s.account, s.kind, s.owner, s.machine = account, kind, owner, machine
	s.calls++
	return environment.VaultScopeState{}, s.err
}
func TestVaultScopeHTTPAuthenticationAndStrictBody(t *testing.T) {
	stub := &vaultScopesHTTPStub{}
	handler := environmentVaultScopeHandler(stub, true)
	request := httptest.NewRequest(http.MethodPut, "/v1/environment/scopes/personal/account_a", jsonReader(t, map[string]any{"operation_id": "op_a", "envelope": "AQ"}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 401 || stub.calls != 0 {
		t.Fatal("unauthenticated write reached storage")
	}
	ctx := context.WithValue(context.Background(), authContextKey{}, principal{User: auth.User{ID: "authenticated_account"}})
	for _, body := range []map[string]any{{"operation_id": "op_a", "envelope": "AQ", "account_id": "attacker"}, {"operation_id": "op_a", "envelope": "AQ", "plaintext": "secret"}} {
		response = httptest.NewRecorder()
		request = httptest.NewRequest(http.MethodPut, "/", jsonReader(t, body)).WithContext(ctx)
		handler.ServeHTTP(response, request)
		if response.Code != 400 || stub.calls != 0 {
			t.Fatal("unknown field accepted")
		}
	}
	request = httptest.NewRequest(http.MethodPut, "/?machine_id=host_a", jsonReader(t, map[string]any{"operation_id": "op_a", "envelope": "AQ"})).WithContext(ctx)
	request.SetPathValue("kind", "personal")
	request.SetPathValue("owner_id", "account_a")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || stub.account != "authenticated_account" || stub.owner != "account_a" || stub.machine != "host_a" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("principal or resource coordinates lost")
	}
	stub.err = environment.ErrKeyAuthorizationRequired
	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPut, "/", jsonReader(t, map[string]any{"operation_id": "op_a", "envelope": "AQ"})).WithContext(ctx)
	handler.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatal("membership failure not forbidden")
	}
}

func TestVaultScopeEntitlementErrorIsActionable(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	writeVaultScopeResult(w, r, nil, environment.ErrTeamEntitlementRequired)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "team_entitlement_required") || !strings.Contains(w.Body.String(), "Billing") {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
}

func TestVaultScopeResultDoesNotMaskMixedOperationalFailures(t *testing.T) {
	const privateCause = "storage-detail-must-not-reach-response"
	for _, expected := range []error{
		environment.ErrTeamEntitlementRequired,
		environment.ErrVaultRotationRequired,
		environment.ErrKeyAuthorizationRequired,
	} {
		t.Run(expected.Error(), func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPut, "/v1/environment/scopes/team/team_a", nil)
			writeVaultScopeResult(w, r, nil, errors.Join(expected, errors.New(privateCause)))
			if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), privateCause) {
				t.Fatalf("mixed failure status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
