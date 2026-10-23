package httpapi

import (
	"net/http/httptest"
	"testing"
)

// A scoped API token reads as its creator, but changes only what its scope is checked on.
func TestScopedTokenRoute(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/v1/favorites", true},
		{"GET", "/v1/machines", true},
		{"POST", "/v1/machines/m1/exec-descriptor", true},
		{"POST", "/v1/machines/m1/ssh-descriptor", true},
		{"POST", "/v1/machines/m1/file-transfer-descriptor", true},
		{"POST", "/v1/machines/m1/terminal-sessions", true},
		{"POST", "/v1/terminal-sessions/s1/connection-descriptor", true},
		{"POST", "/v1/machines/m1/events/wait", true},
		{"POST", "/v1/peer-network/config", true},
		{"PUT", "/v1/ssh/client-keys/abc", true},
		{"POST", "/v1/workspaces/acme/policy/explain", true},
		{"POST", "/v1/auth/api-token-sessions", true},
		{"PUT", "/v1/workspaces/acme/policy", false},
		{"PUT", "/v1/favorites", false},
		{"POST", "/v1/teams/acme/machine-grants", false},
		{"PUT", "/v1/machines/m1/capabilities", false},
		{"DELETE", "/v1/machines/m1", false},
		{"POST", "/v1/machines/m1/terminal-sessions/s1/close", true},
		{"POST", "/v1/terminal-sessions/s1/sharing", false},
	} {
		if got := scopedTokenRoute(httptest.NewRequest(c.method, c.path, nil)); got != c.want {
			t.Errorf("%s %s: %v, want %v", c.method, c.path, got, c.want)
		}
	}
	// pb login registers the session's own E2EE certificate under the account's root; a fresh
	// enrollment can replace that root, which a scoped token never may.
	r := httptest.NewRequest("POST", "/v1/e2ee/bootstrap", nil)
	if !scopedTokenRoute(r) {
		t.Error("a scoped session cannot register its own E2EE certificate")
	}
	r.Header.Set("X-Paperboat-Fresh-Enrollment", "1")
	if scopedTokenRoute(r) {
		t.Error("a scoped session may replace the account's E2EE root")
	}
	if scopedTokenRoute(httptest.NewRequest("POST", "/v1/workspaces/acme/api-tokens", nil)) {
		t.Error("a scoped token may create API tokens")
	}
	if !scopedTokenRoute(httptest.NewRequest("DELETE", "/v1/workspaces/acme/api-tokens/apitok_1", nil)) {
		t.Error("a scoped token cannot revoke API tokens")
	}
}
