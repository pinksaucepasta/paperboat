package teams

import "slices"

// Permission is one administrative right in a team. Roles never imply using
// machines, ENV, previews or tunnels: the workspace policy decides use (PLAN
// F18, F19). The SQL function paperboat.team_role_allows mirrors this matrix;
// TestRoleMatrixMatchesSQL keeps them equal.
type Permission string

const (
	PermissionTeamRead       Permission = "team.read"
	PermissionMembersManage  Permission = "members.manage"
	PermissionSettingsManage Permission = "settings.manage"
	PermissionSecurityManage Permission = "security.manage"
	PermissionAuditRead      Permission = "audit.read"
	PermissionAuditStream    Permission = "audit.stream"
	PermissionBillingManage  Permission = "billing.manage"
	PermissionTeamOwn        Permission = "team.own"
)

// Roles lists every role, most privileged first. "owner" is the team's single
// owner_account; the others are stored on team_members.role.
var Roles = []string{"owner", "admin", "billing", "auditor", "member"}

// Permissions lists every permission in display order.
var Permissions = []Permission{PermissionTeamRead, PermissionMembersManage, PermissionSettingsManage, PermissionSecurityManage, PermissionAuditRead, PermissionAuditStream, PermissionBillingManage, PermissionTeamOwn}

var rolePermissions = map[string][]Permission{
	"owner":   Permissions,
	"admin":   {PermissionTeamRead, PermissionMembersManage, PermissionSettingsManage, PermissionSecurityManage, PermissionAuditRead, PermissionAuditStream},
	"billing": {PermissionTeamRead, PermissionBillingManage},
	"auditor": {PermissionTeamRead, PermissionAuditRead},
	"member":  {PermissionTeamRead},
}

// Allowed reports whether role holds permission. Unknown roles hold nothing.
func Allowed(role string, permission Permission) bool {
	return slices.Contains(rolePermissions[role], permission)
}

// assignableRole reports whether role may be stored on a membership or invitation.
func assignableRole(role string) bool { return role != "owner" && rolePermissions[role] != nil }

// mayManageMember reports whether actor may remove target or change target's role:
// the owner manages everyone else; admins manage only members without member-management rights.
func mayManageMember(actor, target string) bool {
	if target == "owner" || !Allowed(actor, PermissionMembersManage) {
		return false
	}
	return actor == "owner" || !Allowed(target, PermissionMembersManage)
}

// mayGrantRole reports whether actor may give role to someone: only the owner makes admins.
func mayGrantRole(actor, role string) bool {
	if !assignableRole(role) || !Allowed(actor, PermissionMembersManage) {
		return false
	}
	return actor == "owner" || !Allowed(role, PermissionMembersManage)
}

// RoleMatrix is the served form of the matrix (GET /v1/teams/roles).
type RoleMatrix struct {
	Roles       []string            `json:"roles"`
	Permissions []Permission        `json:"permissions"`
	Grants      map[string][]string `json:"grants"`
}

func Matrix() RoleMatrix {
	out := RoleMatrix{Roles: Roles, Permissions: Permissions, Grants: map[string][]string{}}
	for _, role := range Roles {
		out.Grants[role] = []string{}
		for _, p := range rolePermissions[role] {
			out.Grants[role] = append(out.Grants[role], string(p))
		}
	}
	return out
}
