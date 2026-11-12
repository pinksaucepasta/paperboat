-- +goose Up
-- A deleted tunnel kept its name forever: re-creating a tunnel with the name of one that was deleted hours ago answered
-- "a tunnel with this name already exists", and the owner could not even list the one in the way. Names only have to be
-- unique among tunnels that still exist (routes, migration 104, already work this way).
SET LOCAL lock_timeout = '5s';
DROP INDEX paperboat.tunnels_account_id_name_personal;
DROP INDEX paperboat.tunnels_account_id_name_team;
CREATE UNIQUE INDEX tunnels_account_id_name_personal ON paperboat.tunnels(account_id,name) WHERE owner_team_id IS NULL AND deleted_at IS NULL;
CREATE UNIQUE INDEX tunnels_account_id_name_team ON paperboat.tunnels(owner_team_id,name) WHERE owner_team_id IS NOT NULL AND deleted_at IS NULL;

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP INDEX paperboat.tunnels_account_id_name_personal;
DROP INDEX paperboat.tunnels_account_id_name_team;
CREATE UNIQUE INDEX tunnels_account_id_name_personal ON paperboat.tunnels(account_id,name) WHERE owner_team_id IS NULL;
CREATE UNIQUE INDEX tunnels_account_id_name_team ON paperboat.tunnels(owner_team_id,name) WHERE owner_team_id IS NOT NULL;
