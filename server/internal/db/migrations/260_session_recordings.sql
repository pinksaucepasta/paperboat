-- +goose Up
-- The index of team session recordings. The bytes stay on the recording machine; the machine reports start, stop and
-- deletion through its audit backlog. machine_id and machine_name are kept as text: a removed machine's recordings
-- stay listed until retention (config.Retention.Recordings) deletes the rows.
SET LOCAL lock_timeout = '5s';
CREATE TABLE paperboat.session_recordings (
    id text PRIMARY KEY CHECK (id ~ '^rec_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    team_id text NOT NULL REFERENCES paperboat.teams(team_id) ON DELETE CASCADE,
    machine_id text NOT NULL CHECK (length(machine_id) BETWEEN 1 AND 128),
    machine_name text NOT NULL CHECK (length(machine_name) <= 256),
    session_id text NOT NULL CHECK (length(session_id) BETWEEN 1 AND 128),
    trigger text NOT NULL CHECK (trigger IN ('rule', 'manual')),
    started_by text CHECK (length(started_by) BETWEEN 1 AND 128),
    key_generation bigint NOT NULL CHECK (key_generation BETWEEN 1 AND 9007199254740991),
    accounts text[] NOT NULL DEFAULT '{}' CHECK (cardinality(accounts) <= 64),
    started_at timestamptz NOT NULL,
    ended_at timestamptz,
    end_reason text CHECK (end_reason IN ('stopped', 'session_ended', 'quota', 'policy', 'error')),
    bytes bigint NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    segments bigint NOT NULL DEFAULT 0 CHECK (segments >= 0),
    state text NOT NULL DEFAULT 'recording' CHECK (state IN ('recording', 'ended', 'deleted')),
    deleted_at timestamptz,
    CHECK ((trigger = 'manual') = (started_by IS NOT NULL)),
    CHECK ((state = 'deleted') = (deleted_at IS NOT NULL)),
    CHECK (state <> 'ended' OR ended_at IS NOT NULL)
);
CREATE INDEX session_recordings_team ON paperboat.session_recordings (team_id, started_at DESC, id);
CREATE INDEX session_recordings_machine ON paperboat.session_recordings (team_id, machine_id, started_at DESC, id);
CREATE INDEX session_recordings_started ON paperboat.session_recordings (started_at);

-- +goose Down
DROP TABLE paperboat.session_recordings;
