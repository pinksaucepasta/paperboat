package teams

import (
	"os"
	"strings"
	"testing"
)

func TestRoleMatrix(t *testing.T) {
	cases := []struct {
		role string
		want []Permission
	}{
		{"owner", Permissions},
		{"admin", []Permission{PermissionTeamRead, PermissionMembersManage, PermissionSettingsManage, PermissionSecurityManage, PermissionAuditRead, PermissionAuditStream}},
		{"billing", []Permission{PermissionTeamRead, PermissionBillingManage}},
		{"auditor", []Permission{PermissionTeamRead, PermissionAuditRead}},
		{"member", []Permission{PermissionTeamRead}},
		{"stranger", nil},
	}
	for _, c := range cases {
		for _, p := range Permissions {
			want := false
			for _, w := range c.want {
				want = want || w == p
			}
			if got := Allowed(c.role, p); got != want {
				t.Errorf("Allowed(%q, %q) = %v, want %v", c.role, p, got, want)
			}
		}
	}
}

func TestRoleChangesNeverEscalate(t *testing.T) {
	for _, c := range []struct {
		actor, target, role string
		manage, grant       bool
	}{
		{"owner", "admin", "member", true, true},
		{"owner", "member", "admin", true, true},
		{"owner", "owner", "member", false, true},
		{"admin", "member", "billing", true, true},
		{"admin", "auditor", "member", true, true},
		{"admin", "member", "admin", true, false},
		{"admin", "admin", "member", false, true},
		{"billing", "member", "auditor", false, false},
		{"auditor", "member", "auditor", false, false},
		{"member", "member", "member", false, false},
		{"owner", "member", "owner", true, false},
		{"owner", "member", "superuser", true, false},
	} {
		if got := mayManageMember(c.actor, c.target); got != c.manage {
			t.Errorf("mayManageMember(%q, %q) = %v, want %v", c.actor, c.target, got, c.manage)
		}
		if got := mayGrantRole(c.actor, c.role); got != c.grant {
			t.Errorf("mayGrantRole(%q, %q) = %v, want %v", c.actor, c.role, got, c.grant)
		}
	}
}

func TestMatrixServesEveryRole(t *testing.T) {
	m := Matrix()
	if len(m.Roles) != 5 || len(m.Grants["owner"]) != len(Permissions) || len(m.Grants["member"]) != 1 {
		t.Fatalf("matrix = %+v", m)
	}
}

func TestRoleMatrixDocumented(t *testing.T) {
	raw, err := os.ReadFile("../../docs/teams-business.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	start, end := strings.Index(doc, "<!-- roles-matrix:start -->\n"), strings.Index(doc, "<!-- roles-matrix:end -->")
	if start < 0 || end < start {
		t.Fatal("roles matrix markers missing")
	}
	var b strings.Builder
	b.WriteString("| Permission | " + strings.Join(Roles, " | ") + " |\n|---" + strings.Repeat("|---", len(Roles)) + "|\n")
	for _, p := range Permissions {
		b.WriteString("| `" + string(p) + "` |")
		for _, role := range Roles {
			if Allowed(role, p) {
				b.WriteString(" ✓ |")
			} else {
				b.WriteString("  |")
			}
		}
		b.WriteString("\n")
	}
	if got := doc[start+len("<!-- roles-matrix:start -->\n") : end]; got != b.String() {
		t.Fatalf("docs/teams-business.md roles matrix differs from the code; want:\n%s", b.String())
	}
}
