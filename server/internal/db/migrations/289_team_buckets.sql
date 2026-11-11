-- +goose Up
SET LOCAL lock_timeout = '5s';
-- A team's own S3-compatible bucket (PLAN Log 2026-11-11, team bucket): machines archive terminal recordings in it. The
-- secret access key is stored encrypted (internal/secrets) and never returned; only ciphertext is ever put in the bucket,
-- through short-lived presigned requests.
CREATE TABLE paperboat.team_buckets (
    team_id text PRIMARY KEY REFERENCES paperboat.teams(team_id) ON DELETE CASCADE,
    endpoint text NOT NULL CHECK (char_length(endpoint) BETWEEN 8 AND 512),
    region text NOT NULL CHECK (char_length(region) BETWEEN 1 AND 32),
    bucket text NOT NULL CHECK (char_length(bucket) BETWEEN 3 AND 63),
    prefix text NOT NULL DEFAULT '' CHECK (char_length(prefix) <= 128),
    path_style boolean NOT NULL DEFAULT false,
    access_key_id text NOT NULL CHECK (char_length(access_key_id) BETWEEN 1 AND 128),
    secret bytea NOT NULL,
    checked_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE paperboat.team_buckets;
