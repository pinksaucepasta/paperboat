-- +goose Up
-- Runtime carriers must be drained before replacing synthetic identities.
-- +goose StatementBegin
DO $$
BEGIN
 IF EXISTS (SELECT 1 FROM paperboat.runtime_carrier_attachments) THEN
  RAISE EXCEPTION 'drain runtime and preview carriers before applying carrier session identity';
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE paperboat.runtime_carrier_attachments ADD COLUMN preview_session_id text NOT NULL
 CHECK (preview_session_id ~ '^session_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$');
ALTER TABLE paperboat.runtime_carrier_attachments ADD CONSTRAINT runtime_carrier_session_identity_check
 CHECK (session_id ~ '^session_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$');

-- Route readiness uses the same managed-edge authority as pull, observation,
-- and DNS. A valid ready standby still keeps the route reachable.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.runtime_carrier_current(wanted_route text, require_ready boolean) RETURNS boolean
LANGUAGE sql STABLE SET search_path TO 'paperboat', 'pg_catalog' AS $$
 SELECT EXISTS (
 SELECT 1 FROM runtime_carrier_attachments a
 WHERE a.route_id=wanted_route
 AND paperboat.runtime_carrier_valid(a.route_id,a.edge_node_id,a.edge_process_epoch,require_ready)
 );
$$;
-- +goose StatementEnd
