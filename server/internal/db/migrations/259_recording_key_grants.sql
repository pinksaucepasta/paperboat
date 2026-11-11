-- +goose Up
-- One viewer's sealed copy of the team recording key's private bundle, sealed by a client to the viewer's vault
-- sharing key (the same custody as team ENV grants). The server hands it out only to the viewer, only while the
-- policy gives them recording_view on the recording's machine, and only after writing the replay audit event. A
-- viewer who loses replay permission loses the row; history is never re-encrypted. One row per viewer.
SET LOCAL lock_timeout = '5s';
CREATE TABLE paperboat.recording_key_grants (
    team_id text NOT NULL,
    account_id text NOT NULL REFERENCES paperboat.users(id) ON DELETE CASCADE,
    generation bigint NOT NULL,
    recipient_sharing_public bytea NOT NULL CHECK (octet_length(recipient_sharing_public) = 32),
    sealed bytea NOT NULL CHECK (octet_length(sealed) BETWEEN 1 AND 4096),
    granted_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, account_id),
    FOREIGN KEY (team_id, generation) REFERENCES paperboat.recording_keys(team_id, generation) ON DELETE CASCADE
);

-- The accounts a team's policy lets replay somewhere (an allow recording_view rule naming them; denies narrow per
-- machine and are decided at replay). They are who gets a sealed copy; a member who drops out loses theirs.
-- +goose StatementBegin
CREATE FUNCTION paperboat.recording_viewers(team text) RETURNS SETOF text
    LANGUAGE sql STABLE
    SET search_path TO 'paperboat', 'pg_catalog'
    AS $$
 WITH p AS (SELECT id FROM workspace_policies WHERE team_id = team),
 members AS (
  SELECT m.account_id, CASE WHEN t.owner_account = m.account_id THEN 'owner' ELSE m.role END AS role
  FROM teams t JOIN team_members m USING (team_id) WHERE t.team_id = team AND m.active)
 SELECT DISTINCT a.account_id FROM workspace_policy_rules r JOIN p ON r.policy_id = p.id JOIN members a ON
      r.subject_kind = 'all'
   OR (r.subject_kind = 'account' AND r.subject_id = a.account_id)
   OR (r.subject_kind = 'role' AND (r.subject_id = 'member' OR r.subject_id = a.role OR (r.subject_id = 'admin' AND a.role = 'owner')))
   OR (r.subject_kind = 'group' AND EXISTS(SELECT 1 FROM workspace_policy_groups g WHERE g.policy_id = r.policy_id AND g.group_name = r.subject_id AND g.account_id = a.account_id))
 WHERE r.action = 'recording_view' AND NOT r.deny;
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION paperboat.recording_viewers(text);
DROP TABLE paperboat.recording_key_grants;
