-- +goose Up
-- Account deletion (GDPR erasure). The deletion transaction queues provider work here; a worker runs
-- it after commit with retries and a bound, and a failed job never reverts the deletion.
CREATE TABLE paperboat.account_deletion_jobs (
    id text PRIMARY KEY,
    user_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('workos_user', 'stripe_subscription', 'confirmation_mail')),
    target text NOT NULL,
    recipient text NOT NULL DEFAULT '',
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'done', 'failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error text NOT NULL DEFAULT '',
    next_attempt_at timestamp with time zone NOT NULL DEFAULT now(),
    lease_until timestamp with time zone,
    created_at timestamp with time zone NOT NULL DEFAULT now(),
    completed_at timestamp with time zone,
    UNIQUE (user_id, kind, target)
);
CREATE INDEX account_deletion_jobs_due ON paperboat.account_deletion_jobs (next_attempt_at, id) WHERE state = 'queued';

-- Audit events stay append-only, except that the account deletion transaction (which sets
-- paperboat.audit_anonymize) may clear the actor of an event. Only actor columns may change.
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

-- +goose Down
DROP TABLE paperboat.account_deletion_jobs;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.reject_audit_event_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF TG_OP = 'DELETE' AND current_setting('paperboat.audit_prune', true) = 'on'
     AND OLD.created_at < now() - interval '30 days' THEN
    RETURN OLD;
  END IF;
  RAISE EXCEPTION 'audit events are append-only within retention' USING ERRCODE = '55000';
END;
$$;
-- +goose StatementEnd
