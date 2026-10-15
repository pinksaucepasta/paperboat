-- +goose Up
-- Historical rows did not retain the actor. Do not infer a team member from
-- the workspace owner: a current authenticated issuance binds it once.
ALTER TABLE paperboat.usage_accounting_authorities
 ADD COLUMN actor_account_id text
 CHECK (actor_account_id IS NULL OR length(actor_account_id) BETWEEN 1 AND 256);

-- Personal workspace ownership is unambiguous, including retained reports.
UPDATE paperboat.usage_accounting_authorities a
 SET actor_account_id=w.user_id
 FROM paperboat.billing_workspaces w
 WHERE w.id=a.workspace_id AND w.user_id IS NOT NULL;

-- +goose Down
-- Retained accounting identities must not be discarded on a live database.
