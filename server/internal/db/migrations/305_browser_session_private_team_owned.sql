-- +goose Up
SET LOCAL lock_timeout = '5s';
-- A private preview or tunnel on a team-owned machine is opened by its owner as a team member, so its browser session
-- carries the team authority (team, membership, binding and grant generations) that fences it when the membership ends.
-- The baseline check1 demanded no team authority for private, so every owner sign-in to such a preview failed at redeem
-- (postgres_check_violation, edge 503). The all-or-nothing team_authority_check stays; only team mode must carry a team.
-- Added NOT VALID and validated in 306 so this statement holds its lock only briefly.
ALTER TABLE paperboat.browser_access_sessions
    DROP CONSTRAINT browser_access_sessions_check1,
    ADD CONSTRAINT browser_access_sessions_team_mode_check CHECK (access_mode <> 'team' OR team_id IS NOT NULL) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.browser_access_sessions
    DROP CONSTRAINT browser_access_sessions_team_mode_check,
    ADD CONSTRAINT browser_access_sessions_check1 CHECK ((((access_mode = 'private'::text) AND (team_id IS NULL) AND (team_generation IS NULL) AND (membership_generation IS NULL) AND (binding_generation IS NULL) AND (grant_generation IS NULL)) OR ((access_mode = 'team'::text) AND (team_id IS NOT NULL) AND (team_generation IS NOT NULL) AND (membership_generation IS NOT NULL) AND (binding_generation IS NOT NULL) AND (grant_generation IS NOT NULL)))) NOT VALID;
