-- +goose Up
DROP TABLE paperboat.account_network_preferences;
DROP TABLE paperboat.team_network_preferences;

-- +goose Down
-- Network preferences are retired in v1 and their values cannot be restored.
-- +goose StatementBegin
DO $$ BEGIN
    RAISE EXCEPTION 'network preference removal is irreversible; roll back the release instead';
END $$;
-- +goose StatementEnd
