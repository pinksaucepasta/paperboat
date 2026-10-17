-- +goose Up
ALTER TABLE paperboat.team_members ADD COLUMN joined_at timestamptz NOT NULL DEFAULT now();
UPDATE paperboat.team_members m SET joined_at=coalesce(
 (SELECT max(i.accepted_at) FROM paperboat.team_invitations i WHERE i.team_id=m.team_id AND i.recipient_account=m.account_id),
 (SELECT min(a.created_at) FROM paperboat.audit_events a WHERE a.resource_type='team' AND a.resource_id=m.team_id),m.joined_at);
ALTER TABLE paperboat.team_invitations ADD COLUMN role text NOT NULL DEFAULT 'member' CHECK (role IN ('member','admin'));
ALTER TABLE paperboat.team_invitations ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
UPDATE paperboat.team_invitations SET created_at=expires_at-interval '24 hours';

-- +goose Down
ALTER TABLE paperboat.team_invitations DROP COLUMN created_at, DROP COLUMN role;
ALTER TABLE paperboat.team_members DROP COLUMN joined_at;
