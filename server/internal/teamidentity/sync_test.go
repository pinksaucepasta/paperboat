package teamidentity

import "testing"

func TestDesiredMembersFollowGroupRolePrecedence(t *testing.T) {
	state := OrganizationState{Users: []DirectoryUser{
		{ID: "u1", Email: "a@acme.test", Active: true, Groups: []string{"g_audit", "g_admin"}},
		{ID: "u2", Email: "b@acme.test", Active: true, Groups: []string{"g_other"}},
		{ID: "u3", Email: "c@acme.test", Active: false, Groups: []string{"g_admin"}},
		{ID: "u4", Email: "", Active: true},
		{ID: "u5", Email: "d@acme.test", Active: true, Groups: []string{"g_billing", "g_audit"}},
	}}
	roles := map[string]string{"g_admin": "admin", "g_audit": "auditor", "g_billing": "billing"}
	got := desired(state, roles, false)
	want := map[string]string{"u1": "admin", "u2": "member", "u5": "billing"}
	if len(got) != len(want) {
		t.Fatalf("desired = %+v", got)
	}
	for id, role := range want {
		if got[id].Groups[0] != role {
			t.Errorf("%s role = %s, want %s", id, got[id].Groups[0], role)
		}
	}
	if only := desired(state, roles, true); len(only) != 2 || only["u2"].ID != "" {
		t.Fatalf("mapped only = %+v", only)
	}
}
