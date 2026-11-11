-- +goose Up
-- Team session recordings (Teams plan): the public half of each generation of the team recording key. Machines seal
-- each recording's data key to public_key; they pin generation 1 on first use and accept generation N+1 only with a
-- rotation_signature by generation N's signing key (shared/recording VerifyRotation). sealed_previous is generation
-- N's private bundle sealed to generation N+1's public key, so a viewer of the newest generation opens older
-- recordings. Paperboat never holds a private half. One row per rotation (capped in code).
SET LOCAL lock_timeout = '5s';
CREATE TABLE paperboat.recording_keys (
    team_id text NOT NULL REFERENCES paperboat.teams(team_id) ON DELETE CASCADE,
    generation bigint NOT NULL CHECK (generation BETWEEN 1 AND 9007199254740991),
    public_key bytea NOT NULL CHECK (octet_length(public_key) = 32),
    signing_public_key bytea NOT NULL CHECK (octet_length(signing_public_key) = 32),
    rotation_signature bytea CHECK (octet_length(rotation_signature) = 64),
    sealed_previous bytea CHECK (octet_length(sealed_previous) BETWEEN 1 AND 4096),
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, generation),
    CHECK ((generation = 1) = (rotation_signature IS NULL) AND (generation = 1) = (sealed_previous IS NULL))
);

-- +goose Down
DROP TABLE paperboat.recording_keys;
