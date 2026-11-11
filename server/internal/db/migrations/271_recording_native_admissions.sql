-- +goose Up
-- A session recording replay from the CLI: the viewer's CLI endpoint may have no other capability on the recording's
-- machine, so the peer network admits it for capability "recording" until the replay ticket expires (the
-- config_compare_admissions shape). One row per replay ticket (id = its jti); retention deletes expired rows.
SET LOCAL lock_timeout = '5s';
CREATE TABLE paperboat.recording_native_admissions (
    id text PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 128),
    recording_id text NOT NULL CHECK (length(recording_id) BETWEEN 1 AND 128),
    account_id text NOT NULL REFERENCES paperboat.users(id) ON DELETE CASCADE,
    native_cli_session_id text NOT NULL REFERENCES paperboat.cli_client_sessions(id) ON DELETE CASCADE,
    machine_id text NOT NULL REFERENCES paperboat.user_machines(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX recording_native_admissions_cli ON paperboat.recording_native_admissions (native_cli_session_id, account_id);
CREATE INDEX recording_native_admissions_machine ON paperboat.recording_native_admissions (machine_id);
CREATE INDEX recording_native_admissions_expiry ON paperboat.recording_native_admissions (expires_at);

-- +goose Down
DROP TABLE paperboat.recording_native_admissions;
