-- +goose Up
CREATE TABLE paperboat.transactional_mail_outbox (
 mail_id text PRIMARY KEY,
 invitation_id text REFERENCES paperboat.team_invitations(invitation_id) ON DELETE CASCADE,
 payload jsonb NOT NULL,
 state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','sent','failed','cancelled')),
 attempts integer NOT NULL DEFAULT 0,
 first_attempt_at timestamptz,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 lease_until timestamptz,
 accepted_at timestamptz,
 provider_id text,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX transactional_mail_pending ON paperboat.transactional_mail_outbox(next_attempt_at) WHERE state='queued';
CREATE INDEX transactional_mail_invitation_acceptance ON paperboat.transactional_mail_outbox(invitation_id,accepted_at DESC) WHERE invitation_id IS NOT NULL;
ALTER TABLE paperboat.team_invitations ADD COLUMN mail_requested_at timestamptz,
 ADD COLUMN latest_mail_id text REFERENCES paperboat.transactional_mail_outbox(mail_id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE paperboat.team_invitations DROP COLUMN latest_mail_id,DROP COLUMN mail_requested_at;
DROP TABLE paperboat.transactional_mail_outbox;
