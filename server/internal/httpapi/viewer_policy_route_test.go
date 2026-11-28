package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A pb sign-in may change who opens a preview or tunnel. A session made from a scoped API token, or by a machine
// paired from a recipe, may only read it.
func TestScopedAndRecipeSessionsCannotChangeViewerPolicies(t *testing.T) {
	for _, path := range []string{"/v1/browser-access/policies/preview/prv_1/prv_1", "/v1/browser-access/policies/tunnel/tun_1/rte_1"} {
		put := httptest.NewRequest(http.MethodPut, path, nil)
		if scopedTokenRoute(put) || recipeRoute(put, "") || recipeRoute(put, "mch_own") {
			t.Fatalf("PUT %s is open to a scoped or recipe session", path)
		}
	}
}
