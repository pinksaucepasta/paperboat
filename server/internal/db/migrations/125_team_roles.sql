-- +goose Up
-- PLAN F19-01: Billing admin and Auditor roles; one permission matrix.
-- paperboat.team_role_allows mirrors teams.Allowed (TestRoleMatrixMatchesSQL).
ALTER TABLE paperboat.team_members DROP CONSTRAINT team_members_role_check;
ALTER TABLE paperboat.team_members ADD CONSTRAINT team_members_role_check
    CHECK (role IN ('admin', 'billing', 'auditor', 'member'));
ALTER TABLE paperboat.team_invitations DROP CONSTRAINT team_invitations_role_check;
ALTER TABLE paperboat.team_invitations ADD CONSTRAINT team_invitations_role_check
    CHECK (role IN ('admin', 'billing', 'auditor', 'member'));

-- +goose StatementBegin
CREATE FUNCTION paperboat.team_role_allows(role text, permission text) RETURNS boolean
    LANGUAGE sql IMMUTABLE AS $$
SELECT CASE permission
    WHEN 'team.read' THEN role IN ('owner', 'admin', 'billing', 'auditor', 'member')
    WHEN 'members.manage' THEN role IN ('owner', 'admin')
    WHEN 'settings.manage' THEN role IN ('owner', 'admin')
    WHEN 'security.manage' THEN role IN ('owner', 'admin')
    WHEN 'audit.read' THEN role IN ('owner', 'admin', 'auditor')
    WHEN 'audit.stream' THEN role IN ('owner', 'admin')
    WHEN 'billing.manage' THEN role IN ('owner', 'billing')
    WHEN 'team.own' THEN role = 'owner'
    ELSE false
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION paperboat.team_role_allows(text, text);
UPDATE paperboat.team_members SET role='member' WHERE role IN ('billing', 'auditor');
UPDATE paperboat.team_invitations SET role='member' WHERE role IN ('billing', 'auditor');
ALTER TABLE paperboat.team_members DROP CONSTRAINT team_members_role_check;
ALTER TABLE paperboat.team_members ADD CONSTRAINT team_members_role_check CHECK (role IN ('admin', 'member'));
ALTER TABLE paperboat.team_invitations DROP CONSTRAINT team_invitations_role_check;
ALTER TABLE paperboat.team_invitations ADD CONSTRAINT team_invitations_role_check CHECK (role IN ('member', 'admin'));
