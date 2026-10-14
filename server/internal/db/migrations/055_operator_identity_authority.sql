-- +goose Up
-- Operator IDs are opaque application-generated identities. Authentication and
-- foreign keys establish ownership; a retired generator prefix does not.
ALTER TABLE paperboat.operator_accounts DROP CONSTRAINT operator_accounts_id_check;
-- Existing actor columns are shared by customer and operator events. Validate
-- their referenced authoritative row without guessing identity from a prefix.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.check_operator_actor() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.actor_user_id IS NOT NULL
    AND NOT EXISTS (SELECT 1 FROM paperboat.operator_accounts WHERE id=NEW.actor_user_id)
    AND NOT EXISTS (SELECT 1 FROM paperboat.users WHERE id=NEW.actor_user_id) THEN
  RAISE EXCEPTION 'unknown actor identity' USING ERRCODE='23503';
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.check_named_operator_actor() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE actor text := to_jsonb(NEW)->>TG_ARGV[0];
BEGIN
 IF actor IS NOT NULL
    AND NOT EXISTS (SELECT 1 FROM paperboat.operator_accounts WHERE id=actor)
    AND NOT EXISTS (SELECT 1 FROM paperboat.users WHERE id=actor) THEN
  RAISE EXCEPTION 'unknown actor identity' USING ERRCODE='23503';
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 RAISE EXCEPTION 'operator identity authority cutover is irreversible; roll back the release instead';
END $$;
-- +goose StatementEnd
