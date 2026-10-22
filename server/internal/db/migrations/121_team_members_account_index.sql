-- +goose Up
-- Every access check and workspace list looks up an account's teams; team_members was indexed only by
-- (team_id, account_id), so those lookups scanned the table.
CREATE INDEX team_members_account_active ON paperboat.team_members(account_id, team_id) WHERE active;

-- +goose Down
DROP INDEX paperboat.team_members_account_active;
