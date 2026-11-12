-- +goose Up
-- A machine records under one team's policy, key and bucket: the team that owns it, else the team it is shared into
-- (several: the lowest team id, so the answer never flips). A personal machine shared into no team records nothing.
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE FUNCTION paperboat.recording_team(machine text) RETURNS text
    LANGUAGE sql STABLE AS $$
 SELECT coalesce(m.owner_team_id, (SELECT b.team_id FROM paperboat.team_resource_bindings b
   JOIN paperboat.teams t ON t.team_id = b.team_id AND t.deleted_at IS NULL
  WHERE b.resource_kind = 'machine' AND b.resource_id = m.id AND b.active ORDER BY b.team_id LIMIT 1))
 FROM paperboat.user_machines m WHERE m.id = machine;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.publish_team_recording_policy(wanted_teams text[]) RETURNS void
    LANGUAGE sql AS $$
 SELECT paperboat.publish_machine_topic(coalesce((SELECT array_agg(c.id) FROM (
   SELECT m.id FROM paperboat.user_machines m
    WHERE m.owner_team_id = ANY(wanted_teams) AND m.deleted_at IS NULL AND m.revoked_at IS NULL
   UNION
   SELECT m.id FROM paperboat.team_resource_bindings b JOIN paperboat.user_machines m ON m.id = b.resource_id
    WHERE b.team_id = ANY(wanted_teams) AND b.resource_kind = 'machine' AND b.active AND m.owner_team_id IS NULL
      AND m.deleted_at IS NULL AND m.revoked_at IS NULL) c
  WHERE paperboat.recording_team(c.id) = ANY(wanted_teams)
   AND EXISTS(SELECT 1 FROM paperboat.recording_keys k WHERE k.team_id = paperboat.recording_team(c.id))), '{}'::text[]), 'recording_policy');
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'recording_team is forward-only'; END $$;
-- +goose StatementEnd
