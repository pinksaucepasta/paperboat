-- +goose Up
-- control_config_sync_status_history got one row per config sync report and nothing ever read it (the current
-- status lives in control_config_sync_statuses): rows that only grew. The write is gone; so is the table.
DROP TABLE paperboat.control_config_sync_status_history;

-- +goose Down
CREATE TABLE paperboat.control_config_sync_status_history (
    environment_id text NOT NULL REFERENCES paperboat.control_environments(id) ON DELETE CASCADE,
    sync_revision bigint NOT NULL,
    repository_id text NOT NULL,
    assignment_id text NOT NULL,
    machine_id text NOT NULL,
    installation_generation bigint NOT NULL,
    state text NOT NULL,
    error_code text,
    remote_revision text,
    observed_at timestamp with time zone DEFAULT now() NOT NULL,
    PRIMARY KEY (environment_id, sync_revision)
);
