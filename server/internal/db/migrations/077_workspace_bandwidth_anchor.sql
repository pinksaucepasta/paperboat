-- +goose Up
ALTER TABLE paperboat.billing_workspaces ADD COLUMN bandwidth_anchor timestamptz;
UPDATE paperboat.billing_workspaces SET bandwidth_anchor=period_start
WHERE product_code IS NOT NULL AND period_start IS NOT NULL;
ALTER TABLE paperboat.billing_workspace_usage_periods
 ADD COLUMN paid boolean NOT NULL DEFAULT false,
 ADD COLUMN budget_start timestamptz;
UPDATE paperboat.billing_workspace_usage_periods u SET paid=true,budget_start=u.period_start
FROM paperboat.billing_workspaces w
WHERE w.id=u.workspace_id AND w.product_code IS NOT NULL
 AND u.period_start>=w.period_start AND u.period_start<w.period_end;
CREATE INDEX billing_usage_budget ON paperboat.billing_workspace_usage_periods(workspace_id,paid,budget_start);

-- +goose Down
DROP INDEX paperboat.billing_usage_budget;
ALTER TABLE paperboat.billing_workspace_usage_periods DROP COLUMN budget_start,DROP COLUMN paid;
ALTER TABLE paperboat.billing_workspaces DROP COLUMN bandwidth_anchor;
