-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.control_repository_config_sync_revocation() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'paperboat', 'pg_temp'
    AS $$
DECLARE
  environment record;
BEGIN
  IF OLD.state = 'active' AND NEW.state <> 'active' THEN
    FOR environment IN
      SELECT assignment.environment_id
      FROM control_config_assignments assignment
      WHERE assignment.repository_id = NEW.id OR assignment.push_repository_id = NEW.id
    LOOP
      PERFORM revoke_config_sync_for_environment(environment.environment_id);
    END LOOP;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- Retire grants left by the previous pull-only disconnect trigger.
SELECT paperboat.revoke_config_sync_for_environment(a.environment_id)
FROM paperboat.control_config_assignments a
JOIN paperboat.control_config_repositories r ON r.id=a.push_repository_id
WHERE r.state <> 'active' OR r.disconnected_at IS NOT NULL;

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.control_repository_config_sync_revocation() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'paperboat', 'pg_temp'
    AS $$
DECLARE
  environment record;
BEGIN
  IF OLD.state = 'active' AND NEW.state <> 'active' THEN
    FOR environment IN
      SELECT assignment.environment_id
      FROM control_config_assignments assignment
      WHERE assignment.repository_id = NEW.id
    LOOP
      PERFORM revoke_config_sync_for_environment(environment.environment_id);
    END LOOP;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
