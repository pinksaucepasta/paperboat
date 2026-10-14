-- +goose Up
ALTER TABLE paperboat.teams DROP CONSTRAINT teams_suspended_by_fkey;
CREATE TRIGGER teams_operator_suspension_actor
  BEFORE INSERT OR UPDATE OF suspended_by ON paperboat.teams
  FOR EACH ROW EXECUTE FUNCTION paperboat.check_named_operator_actor('suspended_by');
