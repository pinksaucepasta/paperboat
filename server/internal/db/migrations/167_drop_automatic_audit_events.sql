-- +goose Up
-- Config sync credentials and repository access, sync status reports and a preview host's lease renewals were
-- audit events on every automatic sync and renewal: 39.8k rows, 80% of audit_events on 2026-10-23, none of it
-- something a person did. Servers no longer write them (a person's own preview renewal still is one). The audit
-- guard lets the prune job delete only rows past 30 days, so these types get their own narrow exception
-- (paperboat.audit_drop_automatic, these event types only) and the rows written so far go here, once. Rows that
-- servers still rolling out write afterwards age out with the audit retention.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.reject_audit_event_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF TG_OP = 'DELETE' AND current_setting('paperboat.audit_prune', true) = 'on'
     AND OLD.created_at < now() - interval '30 days' THEN
    RETURN OLD;
  END IF;
  -- Automatic events no server writes any more (config syncs, a host's preview renewals, migration 167) may be
  -- deleted at any age, only under their own setting and only these types.
  IF TG_OP = 'DELETE' AND current_setting('paperboat.audit_drop_automatic', true) = 'on'
     AND (OLD.event_type IN ('config.credential_issued', 'config.repository_access_issued', 'config.status_recorded')
       OR (OLD.event_type = 'preview.lease_renewed' AND OLD.actor_type <> 'user')) THEN
    RETURN OLD;
  END IF;
  IF TG_OP = 'UPDATE' AND current_setting('paperboat.audit_anonymize', true) = 'on'
     AND NEW.actor_user_id IS NULL AND NEW.actor_email IS NULL AND NEW.actor_id IS NULL
     AND (to_jsonb(NEW) - ARRAY['actor_type', 'actor_user_id', 'actor_email', 'actor_id', 'account_id'])
       = (to_jsonb(OLD) - ARRAY['actor_type', 'actor_user_id', 'actor_email', 'actor_id', 'account_id']) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'audit events are append-only within retention' USING ERRCODE = '55000';
END;
$$;
-- +goose StatementEnd
SELECT set_config('paperboat.audit_drop_automatic', 'on', true);
DELETE FROM paperboat.audit_events
WHERE event_type IN ('config.credential_issued', 'config.repository_access_issued', 'config.status_recorded')
   OR (event_type = 'preview.lease_renewed' AND actor_type <> 'user');
SELECT set_config('paperboat.audit_drop_automatic', 'off', true);

-- +goose Down
-- The deleted rows are not restored; the guard returns to migration 164's.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.reject_audit_event_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF TG_OP = 'DELETE' AND current_setting('paperboat.audit_prune', true) = 'on'
     AND OLD.created_at < now() - interval '30 days' THEN
    RETURN OLD;
  END IF;
  IF TG_OP = 'UPDATE' AND current_setting('paperboat.audit_anonymize', true) = 'on'
     AND NEW.actor_user_id IS NULL AND NEW.actor_email IS NULL AND NEW.actor_id IS NULL
     AND (to_jsonb(NEW) - ARRAY['actor_type', 'actor_user_id', 'actor_email', 'actor_id', 'account_id'])
       = (to_jsonb(OLD) - ARRAY['actor_type', 'actor_user_id', 'actor_email', 'actor_id', 'account_id']) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'audit events are append-only within retention' USING ERRCODE = '55000';
END;
$$;
-- +goose StatementEnd
