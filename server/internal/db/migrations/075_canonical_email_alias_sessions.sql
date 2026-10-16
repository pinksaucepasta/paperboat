-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.canonical_email_address(value text) RETURNS text
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
 SELECT split_part(split_part(lower(btrim(value)),'@',1),'+',1)||'@'||split_part(lower(btrim(value)),'@',2)
$$;
-- +goose StatementEnd
REINDEX INDEX paperboat.users_current_email_lookup;
UPDATE paperboat.users SET canonical_email=paperboat.canonical_email_address(primary_email)
 WHERE canonical_email IS NOT NULL;
-- Keep the provider identity on the session: several verified WorkOS subjects
-- can sign into the same canonical Paperboat account.
ALTER TABLE paperboat.sessions ADD COLUMN workos_subject text;

-- +goose Down
-- Restoring the refusal policy would split an account's admitted identities.
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'Canonical identity mapping is forward-only; restore a protected database backup to roll back'; END $$;
-- +goose StatementEnd
