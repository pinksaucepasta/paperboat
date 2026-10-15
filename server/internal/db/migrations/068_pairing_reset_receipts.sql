-- +goose Up
CREATE TABLE paperboat.user_machine_pairing_resets (
 enrollment_id text NOT NULL REFERENCES paperboat.user_machine_pairings(id) ON DELETE CASCADE,
 enrollment_generation bigint NOT NULL CHECK (enrollment_generation>0),
 PRIMARY KEY(enrollment_id,enrollment_generation),
 account_id text NOT NULL REFERENCES paperboat.users(id),
 verifier_hash bytea NOT NULL CHECK (octet_length(verifier_hash)=32),
 request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
 retired_machine_ids jsonb NOT NULL CHECK (jsonb_typeof(retired_machine_ids)='array' AND jsonb_array_length(retired_machine_ids) BETWEEN 1 AND 16),
 completed_at timestamptz NOT NULL
);
-- +goose Down
-- Recovery receipts are retained on a live database.
