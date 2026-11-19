-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Replacing a team recording key nobody holds any more (team owner only): the reset generation carries no sealed
-- previous bundle (nobody could open it, so older recordings become unreadable) and no rotation signature; it carries
-- reset_signature instead, by the reset key the previous generation carries (reset_public_key: the team owner's ENV vault
-- writer key when that generation was made). Machines check it exactly like a rotation (shared/recording VerifyReset).
ALTER TABLE paperboat.recording_keys
    ADD COLUMN reset_public_key bytea CHECK (octet_length(reset_public_key) = 32),
    ADD COLUMN reset_signature bytea CHECK (octet_length(reset_signature) = 64),
    DROP CONSTRAINT recording_keys_check,
    ADD CONSTRAINT recording_keys_chain CHECK (
        (generation = 1 AND rotation_signature IS NULL AND reset_signature IS NULL AND sealed_previous IS NULL)
        OR (generation > 1 AND rotation_signature IS NOT NULL AND reset_signature IS NULL AND sealed_previous IS NOT NULL)
        OR (generation > 1 AND rotation_signature IS NULL AND reset_signature IS NOT NULL AND sealed_previous IS NULL)
    ) NOT VALID;

-- +goose Down
-- A reset generation cannot be represented without it; roll back the release instead.
-- +goose StatementBegin
DO $$ BEGIN
    RAISE EXCEPTION 'recording key resets cannot be rolled back; roll back the release instead';
END $$;
-- +goose StatementEnd
