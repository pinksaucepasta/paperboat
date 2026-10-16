-- +goose Up
-- Existing identities are retained, including any legacy aliases or duplicates.
-- Only new signups populate the unique canonical identity column.
-- +goose StatementBegin
CREATE FUNCTION paperboat.canonical_email_address(value text) RETURNS text
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
 SELECT CASE WHEN split_part(lower(btrim(value)),'@',2) IN ('gmail.com','googlemail.com')
 THEN replace(split_part(lower(btrim(value)),'@',1),'.','')||'@gmail.com'
 ELSE lower(btrim(value)) END
$$;
-- +goose StatementEnd
ALTER TABLE paperboat.users ADD COLUMN canonical_email text;
CREATE UNIQUE INDEX users_canonical_email_unique ON paperboat.users(canonical_email)
 WHERE canonical_email IS NOT NULL;
CREATE INDEX users_current_email_lookup
 ON paperboat.users(paperboat.canonical_email_address(primary_email));

-- +goose Down
DROP INDEX paperboat.users_current_email_lookup;
DROP INDEX paperboat.users_canonical_email_unique;
ALTER TABLE paperboat.users DROP COLUMN canonical_email;
DROP FUNCTION paperboat.canonical_email_address(text);
