-- +goose Up
ALTER TABLE paperboat.team_invitations
 ALTER COLUMN recipient_account DROP NOT NULL,
 ADD COLUMN recipient_email text,
 ADD CONSTRAINT team_invitation_recipient CHECK
  (recipient_account IS NOT NULL OR recipient_email IS NOT NULL),
 ADD CONSTRAINT team_invitation_email CHECK
  (recipient_email IS NULL OR (recipient_email=lower(recipient_email)
   AND length(recipient_email) BETWEEN 3 AND 254));
CREATE UNIQUE INDEX team_pending_email_invitation
 ON paperboat.team_invitations(team_id,recipient_email)
 WHERE accepted_at IS NULL AND cancelled_at IS NULL AND recipient_email IS NOT NULL;

-- +goose Down
-- Refuse rollback while an invitation still awaits signup; never discard it.
ALTER TABLE paperboat.team_invitations ALTER COLUMN recipient_account SET NOT NULL;
DROP INDEX paperboat.team_pending_email_invitation;
ALTER TABLE paperboat.team_invitations
 DROP CONSTRAINT team_invitation_recipient,
 DROP CONSTRAINT team_invitation_email,
 DROP COLUMN recipient_email;
