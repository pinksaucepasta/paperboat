-- +goose Up
-- PLAN F19-07: the audit log records who (actor email snapshot, API token), the result of the
-- action (success or denied with a reason) and, for signed-in requests, the client IP and user
-- agent. Retention is per plan (server config); the prune job deletes per workspace plan.
ALTER TABLE paperboat.audit_events
 ADD COLUMN actor_email text,
 ADD COLUMN actor_token_id text,
 ADD COLUMN result text NOT NULL DEFAULT 'success' CHECK (result IN ('success', 'denied')),
 ADD COLUMN reason text CHECK (reason IS NULL OR length(reason) <= 128),
 ADD COLUMN client_ip text CHECK (client_ip IS NULL OR length(client_ip) <= 64),
 ADD COLUMN user_agent text CHECK (user_agent IS NULL OR length(user_agent) <= 256);
ALTER TABLE paperboat.audit_events DISABLE TRIGGER audit_events_append_only;
UPDATE paperboat.audit_events SET result='denied' WHERE event_type LIKE '%.denied';
ALTER TABLE paperboat.audit_events ENABLE TRIGGER audit_events_append_only;
CREATE INDEX audit_events_created ON paperboat.audit_events(created_at, id);
CREATE INDEX audit_events_personal_created ON paperboat.audit_events(created_at, id)
 WHERE activity_workspace_id LIKE 'personal:%';
DROP INDEX paperboat.audit_events_team_activity_expiry;

-- +goose StatementBegin
-- The plan that decides how long a workspace keeps its audit log: team workspaces and events
-- without a workspace keep Team retention; a personal workspace with a paid plan keeps Pro's.
CREATE FUNCTION paperboat.audit_retention_class(workspace text) RETURNS text
 LANGUAGE sql STABLE SET search_path TO 'paperboat', 'pg_catalog' AS $$
 SELECT CASE WHEN workspace IS NULL OR workspace LIKE 'team:%' THEN 'team'
  WHEN EXISTS(SELECT 1 FROM billing_workspaces w WHERE w.user_id=substr(workspace, 10) AND w.team_id IS NULL
   AND billing_workspace_paid_until(w) > now()) THEN 'pro'
  ELSE 'free' END;
$$;

CREATE FUNCTION paperboat.snapshot_audit_actor() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.actor_user_id IS NOT NULL THEN
  SELECT u.primary_email INTO NEW.actor_email FROM paperboat.users u WHERE u.id=NEW.actor_user_id;
 ELSE
  NEW.actor_email := NULL;
 END IF;
 IF NEW.result='success' AND NEW.event_type LIKE '%.denied' THEN
  NEW.result := 'denied';
 END IF;
 RETURN NEW;
END $$;

-- The prune job (activity.Prune) is the only deleter: it sets paperboat.audit_prune for its
-- transaction and deletes per workspace plan. No event younger than the shortest retention
-- (30 days) can ever be deleted, and nothing can be updated.
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
CREATE TRIGGER audit_events_actor_snapshot BEFORE INSERT ON paperboat.audit_events
 FOR EACH ROW EXECUTE FUNCTION paperboat.snapshot_audit_actor();

-- +goose Down
DROP TRIGGER audit_events_actor_snapshot ON paperboat.audit_events;
DROP FUNCTION paperboat.snapshot_audit_actor();
DROP FUNCTION paperboat.audit_retention_class(text);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.reject_audit_event_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF TG_OP = 'DELETE' AND OLD.resource_type = 'team'
     AND OLD.created_at < now() - interval '90 days' THEN
    RETURN OLD;
  END IF;
  RAISE EXCEPTION 'audit events are append-only within retention' USING ERRCODE = '55000';
END;
$$;
-- +goose StatementEnd
CREATE INDEX audit_events_team_activity_expiry ON paperboat.audit_events(created_at, id) WHERE resource_type = 'team';
DROP INDEX paperboat.audit_events_created;
DROP INDEX paperboat.audit_events_personal_created;
ALTER TABLE paperboat.audit_events DROP COLUMN actor_email, DROP COLUMN actor_token_id, DROP COLUMN result,
 DROP COLUMN reason, DROP COLUMN client_ip, DROP COLUMN user_agent;
