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
	if !recipeRoute(httptest.NewRequest("POST", "/v1/machines/m1/ssh-descriptor", nil), "") {
		t.Fatal("a recipe session cannot get a managed SSH operation credential")
	}
	if recipeRoute(httptest.NewRequest("PUT", "/v1/ssh/client-keys/abc", nil), "") {
		t.Fatal("the removed client-key route is allowed")
	}
}

// A recipe machine reaches config repository read access (the handler then allows only its recipe's
// repository), and still no other config write.
func TestRecipeRouteAllowsConfigRepositoryReadAccess(t *testing.T) {
	if !recipeRoute(httptest.NewRequest("POST", "/v1/config-repositories/repository_1/read-access", nil), "") {
		t.Fatal("read access refused")
	}
	if recipeRoute(httptest.NewRequest("POST", "/v1/config-repositories", nil), "") {
		t.Fatal("a recipe machine may not connect repositories")
	}
}
