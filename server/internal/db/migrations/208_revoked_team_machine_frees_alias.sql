-- +goose Up
-- Removing a machine from a team revokes its row but leaves deleted_at NULL, so its alias kept blocking the same
-- name in the team: the next machine moved in (or renamed to it) got machine_name_conflict, deterministically on a
-- repeated test run (C209). A revoked team machine no longer holds its name.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.enforce_user_machine_owner_alias() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF TG_OP = 'UPDATE' AND NEW.alias = OLD.alias AND NEW.owner_team_id IS NOT DISTINCT FROM OLD.owner_team_id AND NEW.user_id=OLD.user_id AND NEW.deleted_at IS NOT DISTINCT FROM OLD.deleted_at THEN RETURN NEW; END IF;
 IF NEW.deleted_at IS NOT NULL THEN RETURN NEW; END IF;
 PERFORM pg_advisory_xact_lock(hashtextextended(CASE WHEN NEW.owner_team_id IS NULL THEN NEW.user_id ELSE 'team:'||NEW.owner_team_id END,0));
 IF (NEW.owner_team_id IS NULL AND EXISTS(SELECT 1 FROM paperboat.user_machines m WHERE m.user_id=NEW.user_id AND lower(m.alias)=lower(NEW.alias)
      AND m.deleted_at IS NULL AND m.owner_team_id IS NULL AND m.id<>NEW.id))
    OR (NEW.owner_team_id IS NOT NULL AND EXISTS(SELECT 1 FROM paperboat.user_machines m WHERE m.owner_team_id=NEW.owner_team_id AND lower(m.alias)=lower(NEW.alias)
      AND m.deleted_at IS NULL AND m.revoked_at IS NULL AND m.id<>NEW.id)) THEN
  RAISE EXCEPTION 'machine alias is already used in this ownership scope' USING ERRCODE='23505', CONSTRAINT='user_machines_owner_alias';
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.enforce_user_machine_owner_alias() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF TG_OP = 'UPDATE' AND NEW.alias = OLD.alias AND NEW.owner_team_id IS NOT DISTINCT FROM OLD.owner_team_id AND NEW.user_id=OLD.user_id AND NEW.deleted_at IS NOT DISTINCT FROM OLD.deleted_at THEN RETURN NEW; END IF;
 IF NEW.deleted_at IS NOT NULL THEN RETURN NEW; END IF;
 PERFORM pg_advisory_xact_lock(hashtextextended(CASE WHEN NEW.owner_team_id IS NULL THEN NEW.user_id ELSE 'team:'||NEW.owner_team_id END,0));
 IF (NEW.owner_team_id IS NULL AND EXISTS(SELECT 1 FROM paperboat.user_machines m WHERE m.user_id=NEW.user_id AND lower(m.alias)=lower(NEW.alias)
      AND m.deleted_at IS NULL AND m.owner_team_id IS NULL AND m.id<>NEW.id))
    OR (NEW.owner_team_id IS NOT NULL AND EXISTS(SELECT 1 FROM paperboat.user_machines m WHERE m.owner_team_id=NEW.owner_team_id AND lower(m.alias)=lower(NEW.alias)
      AND m.deleted_at IS NULL AND m.id<>NEW.id)) THEN
  RAISE EXCEPTION 'machine alias is already used in this ownership scope' USING ERRCODE='23505', CONSTRAINT='user_machines_owner_alias';
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
