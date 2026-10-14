package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/environment"
)

type vaultLayerRouteStub struct {
	layerObservationTestService
	calls, limit, offset int
	source               environment.VaultLayerCoordinate
	account, state       string
}

func (s *vaultLayerRouteStub) VaultLayerRecipients(_ context.Context, account string, source environment.VaultLayerCoordinate, state string, limit, offset int) ([]environment.VaultLayerRecipient, int, error) {
	s.calls++
	s.account, s.source, s.state, s.limit, s.offset = account, source, state, limit, offset
	return []environment.VaultLayerRecipient{}, 0, nil
}
func (*vaultLayerRouteStub) PutVaultLayer(context.Context, string, string, string, environment.VaultLayerPut) (environment.VaultLayerDelivery, error) {
	return environment.VaultLayerDelivery{}, nil
}
func (*vaultLayerRouteStub) VaultLayerContext(context.Context, string, string, string, string) (environment.VaultLayerContext, error) {
	return environment.VaultLayerContext{}, nil
}

func TestVaultLayerRecipientRouteKeepsSourceCoordinatesAndPagination(t *testing.T) {
	s := &vaultLayerRouteStub{}
	mux := http.NewServeMux()
	pass := func(h http.Handler) http.Handler { return h }
	registerEnvironmentVaultLayerRoutes(mux, s, nil, pass, pass)
	request := func(query string) int {
		r := httptest.NewRequest("GET", "/v1/environment/layers?"+query, nil)
		r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "owner"}}))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code
	}
	if code := request("workspace=personal&workspace_id=personal&owner_kind=personal&owner_id=owner&machine_id=&limit=1&offset=2"); code != 200 {
		t.Fatalf("valid scoped inventory rejected: %d", code)
	}
	if s.calls != 1 || s.account != "owner" || s.source.WorkspaceID != "personal" || s.source.OwnerKind != "personal" || s.source.OwnerID != "owner" || s.source.MachineID != "" || s.limit != 1 || s.offset != 2 || s.state != "pending" {
		t.Fatal("source coordinate, paging, or pending default lost")
	}
	for _, query := range []string{
		"workspace=personal&workspace_id=other&owner_kind=personal&owner_id=owner",
		"owner_kind=personal&owner_kind=team&owner_id=owner",
		"owner_kind=personal&owner_id=owner&actor_account_id=foreign",
		"owner_kind=personal&owner_id=owner&limit=201",
		"owner_kind=personal&owner_id=owner&offset=-1",
		"owner_kind=personal&owner_id=owner&state=applied",
	} {
		if code := request(query); code != 400 && code != 403 {
			t.Fatalf("invalid scoped inventory accepted: %d", code)
		}
	}
	if s.calls != 1 {
		t.Fatal("invalid source query reached service")
	}
}
