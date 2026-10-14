-- +goose Up
-- Preview leases are ephemeral. Drain and remove existing leases through their
-- owning cleanup before this v1 cutover; lifecycle kind cannot be inferred.
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM paperboat.preview_leases) THEN
  RAISE EXCEPTION 'drain and remove existing preview leases before owner-session-kind cutover';
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE paperboat.preview_leases ADD COLUMN owner_session_kind text NOT NULL
 CHECK (owner_session_kind IN ('local_lease','foreground','lazy_runtime'));
-- +goose StatementBegin
CREATE FUNCTION paperboat.preview_owner_identity_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.owner_machine_id IS DISTINCT FROM OLD.owner_machine_id
  OR NEW.owner_session_id IS DISTINCT FROM OLD.owner_session_id
  OR NEW.owner_session_kind IS DISTINCT FROM OLD.owner_session_kind THEN
  RAISE EXCEPTION 'preview owner identity is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER preview_owner_identity_immutable BEFORE UPDATE ON paperboat.preview_leases
 FOR EACH ROW EXECUTE FUNCTION paperboat.preview_owner_identity_immutable();
-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'preview lifecycle identity cutover is forward-only'; END $$;
-- +goose StatementEnd
