-- +goose Up
-- An operator can lift a workspace's bandwidth throttle (grant kind throttle_lift): the workspace runs at full speed
-- until the grant ends or is revoked. Added NOT VALID (no row needs a rescan), validated in 424.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.billing_workspace_grants DROP CONSTRAINT billing_workspace_grants_kind_check;
ALTER TABLE paperboat.billing_workspace_grants ADD CONSTRAINT billing_workspace_grants_kind_check
  CHECK (kind IN ('bandwidth','throttle_lift','machines','publications','browser_terminals','public_tcp','seats','viewers_per_terminal')) NOT VALID;

-- Whether an operator's throttle lift runs on the workspace: the debit, the throttle gate and the overview read it.
-- +goose StatementBegin
CREATE FUNCTION paperboat.workspace_throttle_lifted(ws text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM paperboat.billing_workspace_grants
 WHERE workspace_id=ws AND kind='throttle_lift' AND revoked_at IS NULL AND (ends_at IS NULL OR ends_at>now()));
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'throttle lift grant is forward-only'; END $$;
-- +goose StatementEnd
