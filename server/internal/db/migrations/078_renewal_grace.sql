-- +goose Up
ALTER TABLE paperboat.billing_operator_policy ADD COLUMN renewal_grace_seconds integer NOT NULL DEFAULT 604800 CHECK (renewal_grace_seconds>0);
ALTER TABLE paperboat.billing_workspaces ADD COLUMN renewal_grace_until timestamptz,ADD COLUMN renewal_grace_invoice_id text;
-- +goose StatementBegin
CREATE FUNCTION paperboat.billing_workspace_paid_until(w paperboat.billing_workspaces) RETURNS timestamptz LANGUAGE sql STABLE AS $$
 SELECT CASE WHEN w.state IN ('active','trialing') THEN w.period_end
 WHEN w.state IN ('past_due','unpaid') THEN w.renewal_grace_until END;
$$;
CREATE OR REPLACE FUNCTION paperboat.team_paid_entitled(team text) RETURNS boolean
 LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT EXISTS(SELECT 1 FROM billing_workspaces w JOIN teams t ON t.team_id=w.team_id
 WHERE w.team_id=team AND w.user_id IS NULL AND NOT t.suspended AND t.deleted_at IS NULL
 AND w.period_start<=now() AND now()<billing_workspace_paid_until(w));
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION paperboat.team_paid_entitled(team text) RETURNS boolean
 LANGUAGE sql STABLE SET search_path TO 'paperboat','pg_catalog' AS $$
 SELECT EXISTS(SELECT 1 FROM billing_workspaces w JOIN teams t ON t.team_id=w.team_id
 WHERE w.team_id=team AND w.user_id IS NULL AND NOT t.suspended AND t.deleted_at IS NULL
 AND w.state IN ('active','trialing') AND w.period_start<=now() AND now()<w.period_end);
$$;
-- +goose StatementEnd
DROP FUNCTION paperboat.billing_workspace_paid_until(paperboat.billing_workspaces);
ALTER TABLE paperboat.billing_workspaces DROP COLUMN renewal_grace_until,DROP COLUMN renewal_grace_invoice_id;
ALTER TABLE paperboat.billing_operator_policy DROP COLUMN renewal_grace_seconds;
