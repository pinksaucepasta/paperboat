package teamidentity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// WorkOS does not fill a directory user's groups (live, 2026-10-23: both test users had
// "groups": [] while GET /directory_users?group= listed them), so group membership, and with it
// the group → role mapping, comes from listing each group's users.
func TestOrganizationReadsGroupMembershipPerGroup(t *testing.T) {
	user := func(id, email string) map[string]any {
		return map[string]any{"id": id, "email": email, "state": "active", "groups": []any{}}
	}
	pages := map[string]any{
		"/organizations/org_1": map[string]any{"id": "org_1", "domains": []any{}},
		"/connections":         map[string]any{"data": []any{}},
		"/directories":         map[string]any{"data": []any{map[string]any{"id": "dir_1", "type": "generic scim v2.0", "state": "linked"}}},
		"/directory_groups":    map[string]any{"data": []any{map[string]any{"id": "g_eng", "name": "eng"}, map[string]any{"id": "g_fin", "name": "finance"}}},
	}
	users := map[string][]any{
		"dir_1": {user("du_alice", "Alice@Example.test"), user("du_bob", "bob@example.test")},
		"g_eng": {user("du_alice", "alice@example.test")},
		"g_fin": {user("du_bob", "bob@example.test"), user("du_alice", "alice@example.test")},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/directory_users" {
			key := r.URL.Query().Get("directory") + r.URL.Query().Get("group")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": users[key], "list_metadata": map[string]any{}})
			return
		}
		page, ok := pages[r.URL.Path]
		if !ok {
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()
	state, err := HTTPWorkOS{BaseURL: srv.URL, APIKey: "sk_test", Client: srv.Client()}.Organization(context.Background(), "org_1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, u := range state.Users {
		got[u.Email] = u.Groups
	}
	if len(state.Users) != 2 || !slices.Equal(got["alice@example.test"], []string{"g_eng", "g_fin"}) || !slices.Equal(got["bob@example.test"], []string{"g_fin"}) {
		t.Fatalf("users = %+v", state.Users)
	}
}
