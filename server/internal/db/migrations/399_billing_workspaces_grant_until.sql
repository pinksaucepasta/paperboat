-- +goose Up
SET LOCAL lock_timeout = '5s';
-- A plan grant (an operator's support comp) runs until grant_until. While it runs, a Stripe snapshot that is not a live
-- paid subscription is not applied to the workspace, however old the event that carries it (a retry or a manual replay
-- of an old subscription's event): the grant is the one owner of the plan until it ends or the customer really pays.
ALTER TABLE paperboat.billing_workspaces ADD COLUMN grant_until timestamptz;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.billing_workspaces DROP COLUMN grant_until;
