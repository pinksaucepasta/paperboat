package httpapi

import (
	"net/http/httptest"
	"testing"
)

// A recipe machine's pb session asks for managed SSH certificates like any pb session; the removed client-key route
// stays refused.
func TestRecipeRouteAllowsManagedSSHCertificates(t *testing.T) {
	if !recipeRoute(httptest.NewRequest("POST", "/v1/machines/m1/ssh-certificate", nil), "") {
		t.Fatal("a recipe session cannot get a managed SSH certificate")
	}
	if recipeRoute(httptest.NewRequest("PUT", "/v1/ssh/client-keys/abc", nil), "") {
		t.Fatal("the removed client-key route is allowed")
	}
}
